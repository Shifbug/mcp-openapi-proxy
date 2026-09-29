package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rendis/mcp-openapi-proxy/pkg/auth"
	"github.com/rendis/mcp-openapi-proxy/pkg/client"
	"github.com/rendis/mcp-openapi-proxy/pkg/spec"
)

func callEndpoint(ctx context.Context, ep spec.Endpoint, httpClient *client.Client, authResolver *auth.Resolver, cfg Config, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, err := decodeArguments(req)
	if err != nil {
		return proxyErrorResult("invalid_arguments", err.Error(), nil), nil
	}
	return callEndpointArgs(ctx, ep, httpClient, authResolver, cfg, args)
}

func callEndpointArgs(ctx context.Context, ep spec.Endpoint, httpClient *client.Client, authResolver *auth.Resolver, cfg Config, args map[string]any) (*mcp.CallToolResult, error) {
	appliedAuth, err := authResolver.Resolve(ctx, ep.SecurityRequirements)
	if err != nil {
		return proxyErrorResult("auth_required", err.Error(), nil), nil
	}

	baseURL, err := resolveBaseURL(ep, cfg)
	if err != nil {
		return proxyErrorResult("base_url", err.Error(), nil), nil
	}

	pathSection := sectionMap(args, "path")
	querySection := sectionMap(args, "query")
	headerSection := sectionMap(args, "headers")
	cookieSection := sectionMap(args, "cookies")

	serializedPath, err := serializePath(ep.Path, ep.PathParams, pathSection)
	if err != nil {
		return proxyErrorResult("path_serialization", err.Error(), nil), nil
	}

	queryPairs, err := serializeQuery(ep.QueryParams, querySection)
	if err != nil {
		return proxyErrorResult("query_serialization", err.Error(), nil), nil
	}

	headers, err := serializeHeaders(ep.HeaderParams, headerSection)
	if err != nil {
		return proxyErrorResult("header_serialization", err.Error(), nil), nil
	}

	cookies, err := serializeCookies(ep.CookieParams, cookieSection)
	if err != nil {
		return proxyErrorResult("cookie_serialization", err.Error(), nil), nil
	}

	bodyBytes, bodyContentType, err := serializeRequestBody(ep.RequestBody, args["body"])
	if err != nil {
		return proxyErrorResult("request_body", err.Error(), nil), nil
	}
	if bodyContentType != "" {
		headers.Set("Content-Type", bodyContentType)
	}

	queryPairs = mergeAuth(headers, queryPairs, cookies, appliedAuth)

	targetURL, err := buildRequestURL(baseURL, serializedPath, queryPairs)
	if err != nil {
		return proxyErrorResult("request_url", err.Error(), nil), nil
	}
	if err := enforceSecureTransport(targetURL, appliedAuth, cfg.AllowInsecureHTTP); err != nil {
		return proxyErrorResult("insecure_transport", err.Error(), nil), nil
	}

	if len(cookies) > 0 {
		headers.Set("Cookie", formatCookies(cookies))
	}

	resp, err := httpClient.Do(ctx, &client.Request{
		Method:               ep.Method,
		URL:                  targetURL,
		Headers:              headers,
		Body:                 bodyBytes,
		ExpectedContentTypes: expectedResponseContentTypes(ep),
	})
	if err != nil {
		return proxyErrorResult("transport", err.Error(), nil), nil
	}

	envelope := map[string]any{
		"status":       resp.StatusCode,
		"content_type": resp.ContentType,
		"headers":      resp.Headers,
		"body":         resp.Body,
	}
	envelope = redactResponseEnvelope(envelope)
	warnOnResponseSchemaDrift(ep, resp)
	return toolResultWithMedia(envelope, resp.StatusCode >= 400, mediaContentForResponse(resp)), nil
}

func decodeArguments(req *mcp.CallToolRequest) (map[string]any, error) {
	if req == nil || req.Params == nil || req.Params.Arguments == nil {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return nil, fmt.Errorf("unmarshal arguments: %w", err)
	}
	if args == nil {
		args = map[string]any{}
	}
	return args, nil
}

func resolveBaseURL(ep spec.Endpoint, cfg Config) (string, error) {
	if cfg.BaseURL != "" {
		return strings.TrimRight(cfg.BaseURL, "/"), nil
	}
	if ep.BaseURL != "" {
		return strings.TrimRight(ep.BaseURL, "/"), nil
	}
	if len(ep.Servers) == 0 {
		return "", fmt.Errorf("no usable base URL found; set MCP_BASE_URL or declare a single absolute OpenAPI server")
	}
	return "", fmt.Errorf("OpenAPI servers for %s %s are not uniquely usable; set MCP_BASE_URL explicitly", ep.Method, ep.Path)
}

func sectionMap(args map[string]any, key string) map[string]any {
	raw, ok := args[key]
	if !ok || raw == nil {
		return map[string]any{}
	}
	out, ok := raw.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return out
}

func serializePath(template string, params []spec.Param, values map[string]any) (string, error) {
	path := template
	for _, param := range params {
		raw, ok := values[param.Name]
		if !ok {
			continue
		}
		serialized, err := serializePathValue(param, raw)
		if err != nil {
			return "", fmt.Errorf("%s: %w", param.Name, err)
		}
		path = strings.ReplaceAll(path, "{"+param.Name+"}", serialized)
	}
	if strings.Contains(path, "{") {
		return "", fmt.Errorf("missing required path parameters for %q", path)
	}
	return path, nil
}

func serializePathValue(param spec.Param, value any) (string, error) {
	parts, kind := explodeValue(value)
	style := param.Style
	switch style {
	case "", "simple":
		switch kind {
		case valueKindPrimitive:
			return escapePath(parts[0]), nil
		case valueKindArray:
			return joinEscaped(parts, ",", escapePath), nil
		case valueKindObject:
			if param.Explode {
				return joinKeyValue(parts, ",", "=", escapePath), nil
			}
			return joinEscaped(parts, ",", escapePath), nil
		}
	case "label":
		switch kind {
		case valueKindPrimitive:
			return "." + escapePath(parts[0]), nil
		case valueKindArray:
			if param.Explode {
				return "." + joinEscaped(parts, ".", escapePath), nil
			}
			return "." + joinEscaped(parts, ",", escapePath), nil
		case valueKindObject:
			if param.Explode {
				return "." + joinKeyValue(parts, ".", "=", escapePath), nil
			}
			return "." + joinEscaped(parts, ",", escapePath), nil
		}
	case "matrix":
		switch kind {
		case valueKindPrimitive:
			return ";" + param.Name + "=" + escapePath(parts[0]), nil
		case valueKindArray:
			if param.Explode {
				return ";" + repeatPrefixed(param.Name, parts, escapePath), nil
			}
			return ";" + param.Name + "=" + joinEscaped(parts, ",", escapePath), nil
		case valueKindObject:
			if param.Explode {
				return ";" + joinKeyValue(parts, ";", "=", escapePath), nil
			}
			return ";" + param.Name + "=" + joinEscaped(parts, ",", escapePath), nil
		}
	}
	return "", fmt.Errorf("unsupported path serialization style=%q", style)
}

func serializeQuery(params []spec.Param, values map[string]any) ([]queryPair, error) {
	var pairs []queryPair
	for _, param := range params {
		raw, ok := values[param.Name]
		if !ok {
			continue
		}
		paramPairs, err := serializeQueryValue(param, raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", param.Name, err)
		}
		pairs = append(pairs, paramPairs...)
	}
	return pairs, nil
}

func serializeQueryValue(param spec.Param, value any) ([]queryPair, error) {
	parts, kind := explodeValue(value)
	style := param.Style
	if style == "" {
		style = "form"
	}

	switch style {
	case "form":
		switch kind {
		case valueKindPrimitive:
			return []queryPair{{Name: param.Name, Value: primitiveString(value), AllowReserved: param.AllowReserved}}, nil
		case valueKindArray:
			if param.Explode {
				out := make([]queryPair, 0, len(parts))
				for _, part := range parts {
					out = append(out, queryPair{Name: param.Name, Value: part, AllowReserved: param.AllowReserved})
				}
				return out, nil
			}
			return []queryPair{{Name: param.Name, Value: strings.Join(parts, ","), AllowReserved: param.AllowReserved}}, nil
		case valueKindObject:
			if param.Explode {
				out := make([]queryPair, 0, len(parts)/2)
				for i := 0; i < len(parts); i += 2 {
					out = append(out, queryPair{Name: parts[i], Value: parts[i+1], AllowReserved: param.AllowReserved})
				}
				return out, nil
			}
			return []queryPair{{Name: param.Name, Value: strings.Join(parts, ","), AllowReserved: param.AllowReserved}}, nil
		}

	case "spaceDelimited":
		return []queryPair{{Name: param.Name, Value: strings.Join(parts, " "), AllowReserved: param.AllowReserved}}, nil

	case "pipeDelimited":
		return []queryPair{{Name: param.Name, Value: strings.Join(parts, "|"), AllowReserved: param.AllowReserved}}, nil

	case "deepObject":
		obj, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("deepObject requires an object value")
		}
		var out []queryPair
		buildDeepObjectPairs(param.Name, obj, &out, param.AllowReserved)
		return out, nil
	}

	return nil, fmt.Errorf("unsupported query serialization style=%q", style)
}

func serializeHeaders(params []spec.Param, values map[string]any) (http.Header, error) {
	headers := http.Header{}
	for _, param := range params {
		raw, ok := values[param.Name]
		if !ok {
			continue
		}
		serialized, err := serializeHeaderValue(param, raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", param.Name, err)
		}
		headers.Set(param.Name, serialized)
	}
	return headers, nil
}

func serializeHeaderValue(param spec.Param, value any) (string, error) {
	parts, kind := explodeValue(value)
	switch kind {
	case valueKindPrimitive:
		return parts[0], nil
	case valueKindArray:
		return strings.Join(parts, ","), nil
	case valueKindObject:
		if param.Explode {
			return joinKeyValue(parts, ",", "=", identity), nil
		}
		return strings.Join(parts, ","), nil
	default:
		return "", fmt.Errorf("unsupported header value")
	}
}

func serializeCookies(params []spec.Param, values map[string]any) (map[string]string, error) {
	cookies := map[string]string{}
	for _, param := range params {
		raw, ok := values[param.Name]
		if !ok {
			continue
		}
		parts, kind := explodeValue(raw)
		switch kind {
		case valueKindPrimitive:
			cookies[param.Name] = primitiveString(raw)
		case valueKindArray:
			cookies[param.Name] = strings.Join(parts, ",")
		case valueKindObject:
			if param.Explode {
				for i := 0; i < len(parts); i += 2 {
					cookies[parts[i]] = parts[i+1]
				}
			} else {
				cookies[param.Name] = strings.Join(parts, ",")
			}
		default:
			return nil, fmt.Errorf("%s: unsupported cookie value", param.Name)
		}
	}
	return cookies, nil
}

func serializeRequestBody(bodySpec *spec.RequestBody, raw any) ([]byte, string, error) {
	if bodySpec == nil {
		return nil, "", nil
	}

	mt, value, err := selectRequestMediaType(bodySpec, raw)
	if err != nil {
		return nil, "", err
	}
	if mt == nil {
		return nil, "", nil
	}

	contentType := mt.ContentType
	baseContentType, _, _ := mime.ParseMediaType(contentType)

	switch {
	case baseContentType == "application/json" || strings.HasSuffix(baseContentType, "+json"):
		data, err := json.Marshal(value)
		return data, contentType, err

	case baseContentType == "application/x-www-form-urlencoded":
		obj, ok := value.(map[string]any)
		if !ok {
			return nil, "", fmt.Errorf("form-urlencoded body requires an object")
		}
		pairs := []queryPair{}
		for key, val := range obj {
			pairs = append(pairs, queryPair{Name: key, Value: primitiveOrJSON(val)})
		}
		sort.Slice(pairs, func(i, j int) bool {
			return pairs[i].Name < pairs[j].Name
		})
		return []byte(encodeQueryPairs(pairs)), contentType, nil

	case baseContentType == "multipart/form-data":
		obj, ok := value.(map[string]any)
		if !ok {
			return nil, "", fmt.Errorf("multipart body requires an object")
		}
		var buf bytes.Buffer
		writer := multipart.NewWriter(&buf)
		if err := writeMultipartBody(writer, obj, mt.Encoding); err != nil {
			return nil, "", err
		}
		if err := writer.Close(); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), writer.FormDataContentType(), nil

	case isBinaryMediaType(baseContentType):
		data, _, _, err := readBinaryInput(value)
		return data, contentType, err

	case isTextMediaType(baseContentType):
		switch v := value.(type) {
		case string:
			return []byte(v), contentType, nil
		default:
			return []byte(primitiveOrJSON(v)), contentType, nil
		}

	default:
		data, err := json.Marshal(value)
		return data, contentType, err
	}
}

func selectRequestMediaType(bodySpec *spec.RequestBody, raw any) (*spec.MediaType, any, error) {
	if bodySpec == nil || len(bodySpec.Content) == 0 {
		return nil, nil, nil
	}
	if len(bodySpec.Content) == 1 {
		return &bodySpec.Content[0], raw, nil
	}

	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("request body supports multiple media types and requires {content_type, value}")
	}
	contentType, _ := obj["content_type"].(string)
	value, ok := obj["value"]
	if contentType == "" || !ok {
		return nil, nil, fmt.Errorf("request body with multiple media types requires content_type and value")
	}
	for _, mt := range bodySpec.Content {
		if mt.ContentType == contentType {
			return &mt, value, nil
		}
	}
	return nil, nil, fmt.Errorf("unsupported request body media type %q", contentType)
}

func writeMultipartBody(writer *multipart.Writer, obj map[string]any, encodings map[string]spec.Encoding) error {
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		value := obj[key]
		if values, ok := value.([]any); ok {
			for _, item := range values {
				if err := writeMultipartPart(writer, key, item, encodings[key]); err != nil {
					return err
				}
			}
			continue
		}
		if err := writeMultipartPart(writer, key, value, encodings[key]); err != nil {
			return err
		}
	}
	return nil
}

func writeMultipartPart(writer *multipart.Writer, name string, value any, encoding spec.Encoding) error {
	if data, filename, contentType, err := readBinaryInput(value); err == nil {
		header := textproto.MIMEHeader{}
		disposition := fmt.Sprintf(`form-data; name="%s"; filename="%s"`, escapeQuotes(name), escapeQuotes(filename))
		header.Set("Content-Disposition", disposition)
		if contentType == "" {
			contentType = encoding.ContentType
		}
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		header.Set("Content-Type", contentType)
		part, err := writer.CreatePart(header)
		if err != nil {
			return err
		}
		_, err = part.Write(data)
		return err
	}

	if contentType := encoding.ContentType; contentType != "" {
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"`, escapeQuotes(name)))
		header.Set("Content-Type", contentType)
		part, err := writer.CreatePart(header)
		if err != nil {
			return err
		}
		_, err = io.WriteString(part, primitiveOrJSON(value))
		return err
	}

	return writer.WriteField(name, primitiveOrJSON(value))
}

func readBinaryInput(value any) ([]byte, string, string, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, "", "", fmt.Errorf("not a binary input wrapper")
	}

	source, _ := obj["source"].(string)
	switch source {
	case "base64":
		dataBase64, _ := obj["data_base64"].(string)
		if dataBase64 == "" {
			return nil, "", "", fmt.Errorf("binary input with source=base64 requires data_base64")
		}
		data, err := base64.StdEncoding.DecodeString(dataBase64)
		if err != nil {
			return nil, "", "", fmt.Errorf("decode data_base64: %w", err)
		}
		filename, _ := obj["filename"].(string)
		if filename == "" {
			filename = "blob"
		}
		contentType, _ := obj["content_type"].(string)
		return data, filename, contentType, nil

	case "path":
		path, _ := obj["path"].(string)
		if path == "" {
			return nil, "", "", fmt.Errorf("binary input with source=path requires path")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, "", "", fmt.Errorf("read %s: %w", path, err)
		}
		filename, _ := obj["filename"].(string)
		if filename == "" {
			filename = filepath.Base(path)
		}
		contentType, _ := obj["content_type"].(string)
		return data, filename, contentType, nil

	default:
		return nil, "", "", fmt.Errorf("unsupported binary source %q", source)
	}
}

func buildRequestURL(baseURL, path string, pairs []queryPair) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse base URL: %w", err)
	}
	basePath := strings.TrimRight(u.Path, "/")
	switch {
	case path == "":
		u.Path = basePath
	case strings.HasPrefix(path, "/"):
		u.Path = basePath + path
	default:
		u.Path = basePath + "/" + path
	}

	existing := []queryPair{}
	for key, values := range u.Query() {
		for _, value := range values {
			existing = append(existing, queryPair{Name: key, Value: value})
		}
	}
	existing = append(existing, pairs...)
	u.RawQuery = encodeQueryPairs(existing)
	return u.String(), nil
}

func mergeAuth(headers http.Header, queryPairs []queryPair, cookies map[string]string, applied *auth.AppliedAuth) []queryPair {
	if applied == nil {
		return queryPairs
	}
	for key, values := range applied.Headers {
		headers.Del(key)
		for _, value := range values {
			headers.Add(key, value)
		}
	}
	for key, values := range applied.Query {
		next := queryPairs[:0]
		for _, pair := range queryPairs {
			if pair.Name != key {
				next = append(next, pair)
			}
		}
		queryPairs = next
		for _, value := range values {
			queryPairs = append(queryPairs, queryPair{Name: key, Value: value})
		}
	}
	for key, value := range applied.Cookies {
		cookies[key] = value
	}
	return queryPairs
}

func enforceSecureTransport(targetURL string, applied *auth.AppliedAuth, allowInsecure bool) error {
	if allowInsecure || !hasAuth(applied) {
		return nil
	}
	u, err := url.Parse(targetURL)
	if err != nil {
		return fmt.Errorf("parse target URL: %w", err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme != "http" {
		return fmt.Errorf("unsupported URL scheme %q for authenticated request", u.Scheme)
	}
	host := u.Hostname()
	if isLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf("refusing to send credentials over insecure HTTP to %s; set MCP_ALLOW_INSECURE_HTTP=1 to override", u.Host)
}

func hasAuth(applied *auth.AppliedAuth) bool {
	if applied == nil {
		return false
	}
	return len(applied.Headers) > 0 || len(applied.Query) > 0 || len(applied.Cookies) > 0
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func expectedResponseContentTypes(ep spec.Endpoint) []string {
	seen := map[string]bool{}
	var out []string
	for _, resp := range ep.Responses {
		for _, mt := range resp.Content {
			if seen[mt.ContentType] {
				continue
			}
			seen[mt.ContentType] = true
			out = append(out, mt.ContentType)
		}
	}
	return out
}

func warnOnResponseSchemaDrift(ep spec.Endpoint, resp *client.Response) {
	if resp == nil {
		return
	}
	schemaMap := responseSchemaForValidation(ep, resp.StatusCode, resp.ContentType)
	if schemaMap == nil {
		return
	}
	schema := mapToJSONSchema(schemaMap)
	resolved, err := schema.Resolve(nil)
	if err != nil {
		log.Printf("warning: unable to resolve response schema for %s %s: %v", ep.Method, ep.Path, err)
		return
	}
	if err := resolved.Validate(resp.Body); err != nil {
		log.Printf("warning: response schema drift on %s %s (status=%d, content_type=%s): %v", ep.Method, ep.Path, resp.StatusCode, resp.ContentType, err)
	}
}

func responseSchemaForValidation(ep spec.Endpoint, statusCode int, contentType string) map[string]any {
	status := fmt.Sprintf("%d", statusCode)
	var fallback *spec.ResponseInfo
	for _, resp := range ep.Responses {
		if resp.StatusCode == status {
			if schema := selectResponseSchema(resp, contentType); schema != nil {
				return schema
			}
			if len(resp.Content) == 0 {
				return map[string]any{"type": "null"}
			}
		}
		if resp.StatusCode == "default" {
			fallback = &resp
		}
	}
	if fallback != nil {
		return selectResponseSchema(*fallback, contentType)
	}
	return nil
}

func selectResponseSchema(resp spec.ResponseInfo, contentType string) map[string]any {
	if len(resp.Content) == 0 {
		return map[string]any{"type": "null"}
	}
	for _, mt := range resp.Content {
		if mt.ContentType == contentType {
			return adaptOutputSchemaForContentType(mt.Schema, mt.ContentType)
		}
	}
	return adaptOutputSchemaForContentType(resp.Content[0].Schema, resp.Content[0].ContentType)
}

func redactResponseEnvelope(envelope map[string]any) map[string]any {
	redacted, ok := redactSecretValue(envelope).(map[string]any)
	if !ok {
		return envelope
	}
	return redacted
}

func redactSecretValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if isSecretLikeKey(key) {
				out[key] = "[REDACTED]"
				continue
			}
			out[key] = redactSecretValue(item)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(v))
		for key, item := range v {
			if isSecretLikeKey(key) {
				out[key] = "[REDACTED]"
			} else {
				out[key] = item
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactSecretValue(item)
		}
		return out
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
			return v
		}
		var decoded any
		if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
			return v
		}
		redacted := redactSecretValue(decoded)
		encoded, err := json.Marshal(redacted)
		if err != nil {
			return v
		}
		return string(encoded)
	default:
		return value
	}
}

func isSecretLikeKey(key string) bool {
	normalized := strings.ToLower(key)
	normalized = strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(normalized)
	switch normalized {
	case "apikey", "token", "accesstoken", "refreshtoken", "password", "secret", "clientsecret", "cookie":
		return true
	default:
		return false
	}
}

func toolResult(envelope map[string]any, isError bool) *mcp.CallToolResult {
	return toolResultWithMedia(envelope, isError, nil)
}

func toolResultWithMedia(envelope map[string]any, isError bool, extras []mcp.Content) *mcp.CallToolResult {
	text := formatJSON(envelope)
	content := make([]mcp.Content, 0, 1+len(extras))
	content = append(content, &mcp.TextContent{Text: text})
	content = append(content, extras...)
	return &mcp.CallToolResult{
		IsError:           isError,
		StructuredContent: envelope,
		Content:           content,
	}
}

// mediaContentForResponse promotes binary image/audio responses to native MCP
// ImageContent/AudioContent blocks so clients can render them inline. The JSON
// envelope keeps carrying the same body data for backwards compatibility.
func mediaContentForResponse(resp *client.Response) []mcp.Content {
	if resp == nil {
		return nil
	}
	contentType := resp.ContentType
	switch {
	case strings.HasPrefix(contentType, "image/"):
		data, ok := mediaBytesFromBody(resp.Body)
		if !ok {
			return nil
		}
		return []mcp.Content{&mcp.ImageContent{Data: data, MIMEType: contentType}}
	case strings.HasPrefix(contentType, "audio/"):
		data, ok := mediaBytesFromBody(resp.Body)
		if !ok {
			return nil
		}
		return []mcp.Content{&mcp.AudioContent{Data: data, MIMEType: contentType}}
	}
	return nil
}

func mediaBytesFromBody(body any) ([]byte, bool) {
	switch v := body.(type) {
	case client.BinaryBody:
		data, err := base64.StdEncoding.DecodeString(v.DataBase64)
		if err != nil {
			return nil, false
		}
		return data, true
	case string:
		// Text-based media (e.g. image/svg+xml) is decoded as a UTF-8 string.
		return []byte(v), true
	}
	return nil, false
}

func proxyErrorResult(code, message string, details any) *mcp.CallToolResult {
	envelope := map[string]any{
		"status":       0,
		"content_type": "",
		"headers":      map[string][]string{},
		"body":         nil,
		"proxy_error": map[string]any{
			"code":    code,
			"message": message,
			"details": details,
		},
	}
	return toolResult(envelope, true)
}

func formatJSON(v any) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(data)
}

type queryPair struct {
	Name          string
	Value         string
	AllowReserved bool
}

func encodeQueryPairs(pairs []queryPair) string {
	encoded := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		if pair.Name == "" {
			continue
		}
		key := url.QueryEscape(pair.Name)
		val := escapeQuery(pair.Value, pair.AllowReserved)
		encoded = append(encoded, key+"="+val)
	}
	return strings.Join(encoded, "&")
}

func buildDeepObjectPairs(prefix string, value map[string]any, out *[]queryPair, allowReserved bool) {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		name := prefix + "[" + key + "]"
		switch v := value[key].(type) {
		case map[string]any:
			buildDeepObjectPairs(name, v, out, allowReserved)
		case []any:
			for idx, item := range v {
				switch nested := item.(type) {
				case map[string]any:
					buildDeepObjectPairs(fmt.Sprintf("%s[%d]", name, idx), nested, out, allowReserved)
				default:
					*out = append(*out, queryPair{Name: fmt.Sprintf("%s[%d]", name, idx), Value: primitiveString(item), AllowReserved: allowReserved})
				}
			}
		default:
			*out = append(*out, queryPair{Name: name, Value: primitiveString(v), AllowReserved: allowReserved})
		}
	}
}

type valueKind int

const (
	valueKindPrimitive valueKind = iota
	valueKindArray
	valueKindObject
)

func explodeValue(value any) ([]string, valueKind) {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make([]string, 0, len(keys)*2)
		for _, key := range keys {
			out = append(out, key, primitiveString(v[key]))
		}
		return out, valueKindObject
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, primitiveString(item))
		}
		return out, valueKindArray
	default:
		return []string{primitiveString(value)}, valueKindPrimitive
	}
}

func primitiveString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	default:
		return fmt.Sprintf("%v", v)
	}
}

func primitiveOrJSON(value any) string {
	switch value.(type) {
	case string, float64, bool, int, int64:
		return primitiveString(value)
	default:
		data, err := json.Marshal(value)
		if err != nil {
			return primitiveString(value)
		}
		return string(data)
	}
}

func joinEscaped(parts []string, sep string, escape func(string) string) string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		out = append(out, escape(part))
	}
	return strings.Join(out, sep)
}

func joinKeyValue(parts []string, pairSep, kvSep string, escape func(string) string) string {
	out := make([]string, 0, len(parts)/2)
	for i := 0; i < len(parts); i += 2 {
		out = append(out, escape(parts[i])+kvSep+escape(parts[i+1]))
	}
	return strings.Join(out, pairSep)
}

func repeatPrefixed(name string, parts []string, escape func(string) string) string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		out = append(out, name+"="+escape(part))
	}
	return strings.Join(out, ";")
}

func escapePath(v string) string {
	return url.PathEscape(v)
}

func escapeQuery(v string, allowReserved bool) string {
	escaped := url.QueryEscape(v)
	if !allowReserved {
		return escaped
	}
	replacer := strings.NewReplacer(
		"%2F", "/",
		"%3A", ":",
		"%3F", "?",
		"%23", "#",
		"%5B", "[",
		"%5D", "]",
		"%40", "@",
		"%21", "!",
		"%24", "$",
		"%26", "&",
		"%27", "'",
		"%28", "(",
		"%29", ")",
		"%2A", "*",
		"%2B", "+",
		"%2C", ",",
		"%3B", ";",
		"%3D", "=",
	)
	return replacer.Replace(escaped)
}

func formatCookies(cookies map[string]string) string {
	keys := make([]string, 0, len(cookies))
	for key := range cookies {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+cookies[key])
	}
	return strings.Join(parts, "; ")
}

func identity(v string) string { return v }

func escapeQuotes(v string) string {
	return strings.ReplaceAll(v, `"`, `\"`)
}
