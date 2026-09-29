package spec

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"github.com/getkin/kin-openapi/openapi3"
)

const (
	methodHead    = "HEAD"
	methodOptions = "OPTIONS"
)

// LoadSpec loads and normalizes an OpenAPI 3.x spec from a local file path or
// HTTP(S) URL. Validation is performed when validate is true.
func LoadSpec(source string, validate bool) ([]Endpoint, *openapi3.T, error) {
	if doc, detected, err := loadSwagger2AsOpenAPI3(source); detected {
		if err != nil {
			return nil, nil, err
		}
		if validate {
			if err := doc.Validate(context.Background()); err != nil {
				return nil, nil, fmt.Errorf("validate converted Swagger 2 spec %s: %w", source, err)
			}
		}
		return extractEndpoints(doc), doc, nil
	}

	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true

	var (
		doc *openapi3.T
		err error
	)

	if isHTTP(source) {
		u, parseErr := url.Parse(source)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("parse spec URL: %w", parseErr)
		}
		loader.ReadFromURIFunc = httpReadFromURI
		doc, err = loader.LoadFromURI(u)
	} else {
		doc, err = loader.LoadFromFile(source)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("load spec from %s: %w", source, err)
	}

	if validate {
		if err := doc.Validate(context.Background()); err != nil {
			return nil, nil, fmt.Errorf("validate spec %s: %w", source, err)
		}
	}

	return extractEndpoints(doc), doc, nil
}

// loadSwagger2AsOpenAPI3 detects a JSON Swagger 2.0 document and normalizes it
// through kin-openapi's converter. Non-Swagger inputs return detected=false so
// the existing OpenAPI 3 loader remains the canonical path.
func loadSwagger2AsOpenAPI3(source string) (*openapi3.T, bool, error) {
	data, err := readSpecSource(source)
	if err != nil {
		return nil, false, nil
	}
	var probe struct {
		Swagger string `json:"swagger"`
	}
	if err := json.Unmarshal(data, &probe); err != nil || probe.Swagger != "2.0" {
		return nil, false, nil
	}
	var doc2 openapi2.T
	if err := json.Unmarshal(data, &doc2); err != nil {
		return nil, true, fmt.Errorf("parse Swagger 2 spec %s: %w", source, err)
	}
	doc3, err := openapi2conv.ToV3(&doc2)
	if err != nil {
		return nil, true, fmt.Errorf("convert Swagger 2 spec %s: %w", source, err)
	}
	return doc3, true, nil
}

func readSpecSource(source string) ([]byte, error) {
	if isHTTP(source) {
		u, err := url.Parse(source)
		if err != nil {
			return nil, err
		}
		return httpReadFromURI(nil, u)
	}
	return os.ReadFile(source)
}

// CollectOAuthScopes returns all scopes referenced by security requirements in
// sorted order. It includes document-level and operation-level requirements.
func CollectOAuthScopes(doc *openapi3.T) []string {
	if doc == nil || doc.Paths == nil {
		return nil
	}

	seen := map[string]bool{}
	addScopes := func(reqs openapi3.SecurityRequirements) {
		for _, req := range reqs {
			for _, scopes := range req {
				for _, scope := range scopes {
					if scope != "" {
						seen[scope] = true
					}
				}
			}
		}
	}

	addScopes(doc.Security)

	for _, pathItem := range doc.Paths.Map() {
		if pathItem == nil {
			continue
		}
		for _, method := range supportedMethods() {
			op := pathItem.GetOperation(method)
			if op == nil || op.Security == nil {
				continue
			}
			addScopes(*op.Security)
		}
	}

	scopes := make([]string, 0, len(seen))
	for scope := range seen {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	return scopes
}

// extractEndpoints walks doc.Paths and extracts all supported operations.
func extractEndpoints(doc *openapi3.T) []Endpoint {
	if doc == nil || doc.Paths == nil {
		return nil
	}

	pathMap := doc.Paths.Map()
	paths := make([]string, 0, len(pathMap))
	for p := range pathMap {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var endpoints []Endpoint
	for _, path := range paths {
		pathItem := pathMap[path]
		if pathItem == nil {
			continue
		}

		for _, method := range supportedMethods() {
			op := pathItem.GetOperation(method)
			if op == nil {
				continue
			}

			ep := Endpoint{
				Method:      method,
				Path:        path,
				OperationID: op.OperationID,
				Summary:     op.Summary,
				Description: op.Description,
				Tags:        op.Tags,
				Deprecated:  op.Deprecated,
			}

			var opServers openapi3.Servers
			if op.Servers != nil {
				opServers = *op.Servers
			}
			ep.BaseURL, ep.Servers = resolveEndpointServers(doc.Servers, pathItem.Servers, opServers)

			allParams := mergeParameters(pathItem.Parameters, op.Parameters)
			for _, pRef := range allParams {
				if pRef == nil || pRef.Value == nil {
					continue
				}
				param := convertParameter(pRef.Value, method, path)
				switch pRef.Value.In {
				case openapi3.ParameterInPath:
					ep.PathParams = append(ep.PathParams, param)
				case openapi3.ParameterInQuery:
					ep.QueryParams = append(ep.QueryParams, param)
				case openapi3.ParameterInHeader:
					ep.HeaderParams = append(ep.HeaderParams, param)
				case openapi3.ParameterInCookie:
					log.Printf("warning: cookie parameter %q on %s %s requires explicit cookie handling", pRef.Value.Name, method, path)
					ep.CookieParams = append(ep.CookieParams, param)
				}
			}

			ep.RequestBody = extractRequestBody(op.RequestBody)
			ep.Responses = extractResponses(op)
			ep.ExternalDocs = externalDocsURL(op.ExternalDocs)
			ep.SecurityRequirements = extractSecurityRequirements(op.Security, doc.Security, doc)
			ep.Security, ep.SecurityInfo = flattenSecurity(ep.SecurityRequirements)

			endpoints = append(endpoints, ep)
		}
	}

	return endpoints
}

func supportedMethods() []string {
	return []string{
		http.MethodGet,
		http.MethodPost,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
		methodHead,
		methodOptions,
	}
}

// mergeParameters combines path-level and operation-level parameters.
// Operation-level parameters override path-level ones with the same name+in.
func mergeParameters(pathParams, opParams openapi3.Parameters) openapi3.Parameters {
	seen := make(map[string]bool)
	var result openapi3.Parameters

	for _, p := range opParams {
		if p != nil && p.Value != nil {
			seen[p.Value.In+":"+p.Value.Name] = true
		}
		result = append(result, p)
	}
	for _, p := range pathParams {
		if p == nil || p.Value == nil {
			continue
		}
		key := p.Value.In + ":" + p.Value.Name
		if seen[key] {
			continue
		}
		result = append(result, p)
	}
	return result
}

func convertParameter(p *openapi3.Parameter, method, path string) Param {
	sm, _ := p.SerializationMethod()
	style, explode := "", false
	if sm != nil {
		style = sm.Style
		explode = sm.Explode
	}

	schemaMap := schemaRefToMap(p.Schema)
	contentType := ""
	if schemaMap == nil && len(p.Content) > 0 {
		mt := firstMediaType(extractMediaTypes(p.Content))
		if mt != nil {
			schemaMap = cloneMap(mt.Schema)
			contentType = mt.ContentType
		}
	}
	schemaMap = mergeExamplesIntoSchema(schemaMap, p.Example, examplesToSlice(p.Examples))

	param := Param{
		Name:            p.Name,
		Description:     p.Description,
		Required:        p.Required,
		Type:            schemaType(p.Schema, schemaMap),
		Default:         schemaDefault(p.Schema),
		Format:          schemaFormat(p.Schema, schemaMap),
		Style:           style,
		Explode:         explode,
		AllowReserved:   p.AllowReserved,
		AllowEmptyValue: p.AllowEmptyValue,
		Deprecated:      p.Deprecated,
		Schema:          schemaMap,
		ContentType:     contentType,
		Examples:        examplesFromSchemaOrParam(schemaMap, p.Example, p.Examples),
	}

	if p.Schema != nil && p.Schema.Value != nil {
		s := p.Schema.Value
		param.Enum = append([]any(nil), s.Enum...)
		param.Minimum = s.Min
		param.Maximum = s.Max
		if s.MinLength != 0 {
			ml := s.MinLength
			param.MinLength = &ml
		}
		param.MaxLength = s.MaxLength
	}

	return param
}

func extractRequestBody(ref *openapi3.RequestBodyRef) *RequestBody {
	if ref == nil || ref.Value == nil {
		return nil
	}
	body := &RequestBody{
		Required: ref.Value.Required,
		Content:  extractMediaTypes(ref.Value.Content),
	}
	if len(body.Content) == 0 {
		return nil
	}
	return body
}

// extractBodySchema returns the preferred request body content type and schema.
// It is kept as a compatibility helper for tests.
func extractBodySchema(rb *openapi3.RequestBody) (string, map[string]any) {
	if rb == nil || rb.Content == nil {
		return "", nil
	}
	for _, ct := range []string{"application/json", "application/merge-patch+json"} {
		if mt := rb.Content[ct]; mt != nil && mt.Schema != nil {
			return ct, schemaRefToMap(mt.Schema)
		}
	}
	for ct, mt := range rb.Content {
		if strings.HasPrefix(ct, "multipart/") || strings.HasPrefix(ct, "application/x-www-form") {
			continue
		}
		if mt != nil && mt.Schema != nil {
			return ct, schemaRefToMap(mt.Schema)
		}
	}
	return "", nil
}

func extractResponses(op *openapi3.Operation) []ResponseInfo {
	if op == nil || op.Responses == nil {
		return nil
	}

	respMap := op.Responses.Map()
	codes := make([]string, 0, len(respMap))
	for code := range respMap {
		codes = append(codes, code)
	}
	sort.Strings(codes)

	var responses []ResponseInfo
	for _, code := range codes {
		ref := respMap[code]
		if ref == nil || ref.Value == nil {
			continue
		}
		resp := ref.Value
		ri := ResponseInfo{
			StatusCode:  code,
			Description: derefString(resp.Description),
			Content:     extractMediaTypes(resp.Content),
		}
		for headerName, headerRef := range resp.Headers {
			if headerRef == nil || headerRef.Value == nil {
				continue
			}
			ri.Headers = append(ri.Headers, ResponseHeader{
				Name:        headerName,
				Description: headerRef.Value.Description,
				Required:    headerRef.Value.Required,
				Schema:      schemaRefToMap(headerRef.Value.Schema),
			})
		}
		sort.Slice(ri.Headers, func(i, j int) bool {
			return ri.Headers[i].Name < ri.Headers[j].Name
		})
		responses = append(responses, ri)
	}

	return responses
}

func extractMediaTypes(content openapi3.Content) []MediaType {
	if len(content) == 0 {
		return nil
	}

	keys := make([]string, 0, len(content))
	for ct := range content {
		keys = append(keys, ct)
	}
	sort.Slice(keys, func(i, j int) bool {
		return mediaTypeRank(keys[i]) < mediaTypeRank(keys[j]) ||
			(mediaTypeRank(keys[i]) == mediaTypeRank(keys[j]) && keys[i] < keys[j])
	})

	mediaTypes := make([]MediaType, 0, len(keys))
	for _, ct := range keys {
		mt := content[ct]
		if mt == nil {
			continue
		}

		encodings := make(map[string]Encoding, len(mt.Encoding))
		for name, enc := range mt.Encoding {
			if enc == nil {
				continue
			}
			sm := enc.SerializationMethod()
			if sm == nil {
				continue
			}
			encodings[name] = Encoding{
				ContentType:   enc.ContentType,
				Style:         sm.Style,
				Explode:       sm.Explode,
				AllowReserved: enc.AllowReserved,
			}
		}

		mediaTypes = append(mediaTypes, MediaType{
			ContentType: ct,
			Schema:      mergeExamplesIntoSchema(schemaRefToMap(mt.Schema), mt.Example, examplesToSlice(mt.Examples)),
			Examples:    examplesToSlice(mt.Examples),
			Encoding:    encodings,
		})
	}

	return mediaTypes
}

func resolveEndpointServers(docServers, pathServers, opServers openapi3.Servers) (string, []ServerInfo) {
	servers := docServers
	switch {
	case len(opServers) > 0:
		servers = opServers
	case len(pathServers) > 0:
		servers = pathServers
	}

	infos := resolveServerInfos(servers)
	if len(infos) == 1 && isUsableServerURL(infos[0].URL) {
		return infos[0].URL, infos
	}
	return "", infos
}

func resolveServerInfos(servers openapi3.Servers) []ServerInfo {
	if len(servers) == 0 {
		return nil
	}

	seen := map[string]bool{}
	infos := make([]ServerInfo, 0, len(servers))
	for _, srv := range servers {
		if srv == nil {
			continue
		}
		resolved := resolveServerURL(srv)
		if resolved == "" || seen[resolved] {
			continue
		}
		seen[resolved] = true
		infos = append(infos, ServerInfo{
			URL:         resolved,
			Description: srv.Description,
		})
	}
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].URL < infos[j].URL
	})
	return infos
}

func resolveServerURL(server *openapi3.Server) string {
	if server == nil || server.URL == "" {
		return ""
	}
	resolved := server.URL
	for name, variable := range server.Variables {
		if variable == nil || variable.Default == "" {
			return ""
		}
		resolved = strings.ReplaceAll(resolved, "{"+name+"}", variable.Default)
	}
	return resolved
}

func isUsableServerURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.IsAbs() && (u.Scheme == "http" || u.Scheme == "https")
}

func extractSecurityRequirements(opSecurity *openapi3.SecurityRequirements, docSecurity openapi3.SecurityRequirements, doc *openapi3.T) []SecurityRequirement {
	reqs := docSecurity
	if opSecurity != nil {
		reqs = *opSecurity
	}
	if len(reqs) == 0 {
		return nil
	}

	out := make([]SecurityRequirement, 0, len(reqs))
	for _, req := range reqs {
		if len(req) == 0 {
			out = append(out, SecurityRequirement{})
			continue
		}
		names := make([]string, 0, len(req))
		for name := range req {
			names = append(names, name)
		}
		sort.Strings(names)

		sr := SecurityRequirement{}
		for _, name := range names {
			info := SecurityInfo{Name: name, Scopes: append([]string(nil), req[name]...)}
			if doc != nil && doc.Components != nil && doc.Components.SecuritySchemes != nil {
				if ref, ok := doc.Components.SecuritySchemes[name]; ok && ref != nil && ref.Value != nil {
					scheme := ref.Value
					info.Type = scheme.Type
					info.In = scheme.In
					info.ParameterName = scheme.Name
					info.Scheme = scheme.Scheme
					info.BearerFormat = scheme.BearerFormat
					info.Description = scheme.Description
					info.OpenIDConnectURL = scheme.OpenIdConnectUrl
				}
			}
			sr.Schemes = append(sr.Schemes, info)
		}
		out = append(out, sr)
	}

	return out
}

func flattenSecurity(requirements []SecurityRequirement) ([]string, []SecurityInfo) {
	if len(requirements) == 0 {
		return nil, nil
	}

	seen := map[string]bool{}
	var names []string
	var infos []SecurityInfo
	for _, req := range requirements {
		for _, scheme := range req.Schemes {
			if seen[scheme.Name] {
				continue
			}
			seen[scheme.Name] = true
			names = append(names, scheme.Name)
			infos = append(infos, scheme)
		}
	}
	return names, infos
}

// extractSecurityNames is kept as a compatibility helper for tests.
func extractSecurityNames(opSecurity *openapi3.SecurityRequirements, docSecurity openapi3.SecurityRequirements) []string {
	names, _ := flattenSecurity(extractSecurityRequirements(opSecurity, docSecurity, nil))
	return names
}

func schemaRefToMap(ref *openapi3.SchemaRef) map[string]any {
	if ref == nil || ref.Value == nil {
		return nil
	}
	data, err := json.Marshal(ref.Value)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return normalizeSchemaMap(m)
}

func normalizeSchemaMap(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	normalized, ok := normalizeValue(schema).(map[string]any)
	if !ok {
		return nil
	}
	return normalized
}

func normalizeValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, vv := range x {
			if k == "__origin__" {
				continue
			}
			m[k] = normalizeValue(vv)
		}

		if example, ok := m["example"]; ok {
			if _, exists := m["examples"]; !exists {
				m["examples"] = []any{normalizeValue(example)}
			}
			delete(m, "example")
		}

		if nullable, ok := m["nullable"].(bool); ok && nullable {
			delete(m, "nullable")
			switch t := m["type"].(type) {
			case string:
				m["type"] = []any{t, "null"}
			case []any:
				if !containsAny(t, "null") {
					m["type"] = append(t, "null")
				}
			case []string:
				if !containsString(t, "null") {
					next := append(append([]string(nil), t...), "null")
					m["type"] = next
				}
			default:
				m = map[string]any{
					"anyOf": []any{m, map[string]any{"type": "null"}},
				}
			}
		}

		return m
	case []any:
		out := make([]any, 0, len(x))
		for _, item := range x {
			out = append(out, normalizeValue(item))
		}
		return out
	case []string:
		out := make([]any, 0, len(x))
		for _, item := range x {
			out = append(out, item)
		}
		return out
	default:
		return x
	}
}

func mergeExamplesIntoSchema(schema map[string]any, example any, examples []any) map[string]any {
	if schema == nil {
		return nil
	}
	schema = cloneMap(schema)
	if len(examples) > 0 {
		schema["examples"] = examples
		return schema
	}
	if example != nil {
		schema["examples"] = []any{normalizeValue(example)}
	}
	return schema
}

func examplesFromSchemaOrParam(schema map[string]any, example any, examples openapi3.Examples) []any {
	if v, ok := schema["examples"].([]any); ok && len(v) > 0 {
		return append([]any(nil), v...)
	}
	return examplesToSlice(examplesWithInline(example, examples))
}

func examplesWithInline(example any, examples openapi3.Examples) openapi3.Examples {
	if example == nil {
		return examples
	}
	out := openapi3.Examples{}
	for name, ref := range examples {
		out[name] = ref
	}
	out["default"] = &openapi3.ExampleRef{Value: &openapi3.Example{Value: example}}
	return out
}

func examplesToSlice(examples openapi3.Examples) []any {
	if len(examples) == 0 {
		return nil
	}
	names := make([]string, 0, len(examples))
	for name := range examples {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]any, 0, len(names))
	for _, name := range names {
		ref := examples[name]
		if ref == nil || ref.Value == nil {
			continue
		}
		out = append(out, normalizeValue(ref.Value.Value))
	}
	return out
}

func externalDocsURL(docs *openapi3.ExternalDocs) string {
	if docs == nil {
		return ""
	}
	return docs.URL
}

func schemaType(ref *openapi3.SchemaRef, schema map[string]any) string {
	if ref != nil && ref.Value != nil && ref.Value.Type != nil {
		if types := ref.Value.Type.Slice(); len(types) > 0 {
			return types[0]
		}
	}
	switch t := schema["type"].(type) {
	case string:
		return t
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && s != "null" {
				return s
			}
		}
	}
	return "string"
}

func schemaFormat(ref *openapi3.SchemaRef, schema map[string]any) string {
	if ref != nil && ref.Value != nil {
		return ref.Value.Format
	}
	if format, ok := schema["format"].(string); ok {
		return format
	}
	return ""
}

func schemaDefault(ref *openapi3.SchemaRef) any {
	if ref == nil || ref.Value == nil {
		return nil
	}
	return ref.Value.Default
}

func mediaTypeRank(contentType string) int {
	base, _, _ := mime.ParseMediaType(contentType)
	switch {
	case base == "application/json":
		return 0
	case strings.HasSuffix(base, "+json"):
		return 1
	case strings.HasPrefix(base, "text/"):
		return 2
	case isBinaryContentType(base):
		return 3
	default:
		return 4
	}
}

func isBinaryContentType(contentType string) bool {
	base, _, _ := mime.ParseMediaType(contentType)
	switch {
	case base == "application/octet-stream":
		return true
	case strings.HasPrefix(base, "image/"):
		return true
	case strings.HasPrefix(base, "audio/"):
		return true
	case strings.HasPrefix(base, "video/"):
		return true
	case base == "application/pdf":
		return true
	case base == "application/zip":
		return true
	default:
		return false
	}
}

func firstMediaType(mediaTypes []MediaType) *MediaType {
	if len(mediaTypes) == 0 {
		return nil
	}
	return &mediaTypes[0]
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		out := make(map[string]any, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}

func containsAny(items []any, want string) bool {
	for _, item := range items {
		if s, ok := item.(string); ok && s == want {
			return true
		}
	}
	return false
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func isHTTP(source string) bool {
	return strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
}

func httpReadFromURI(_ *openapi3.Loader, location *url.URL) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(location.String())
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", location, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d", location, resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response from %s: %w", location, err)
	}
	return data, nil
}
