package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rendis/mcp-openapi-proxy/pkg/auth"
	"github.com/rendis/mcp-openapi-proxy/pkg/client"
	"github.com/rendis/mcp-openapi-proxy/pkg/spec"
)

func newHandler(ep spec.Endpoint, cfg Config) mcp.ToolHandler {
	httpClient := client.New(nil, 1<<20)
	authResolver := auth.NewResolver(cfg.AuthProfile)
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return callEndpoint(ctx, ep, httpClient, authResolver, cfg, req)
	}
}

func TestBuildHandler_JSONEnvelopeAndQuerySerialization(t *testing.T) {
	var gotQuery string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer api.Close()

	ep := loadEndpoint(t, "/pets", "GET", "../../testdata/petstore.yaml")
	handler := newHandler(ep, Config{BaseURL: api.URL, ToolPrefix: "api", AuthProfile: "default"})
	result, err := handler(context.Background(), toolRequest(t, map[string]any{
		"query": map[string]any{
			"limit": 10,
			"tags":  []any{"cat", "dog"},
		},
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %#v", envelopeFromResult(t, result))
	}
	env := envelopeFromResult(t, result)
	if statusCode(t, env) != 200 {
		t.Fatalf("status = %#v", env["status"])
	}
	if env["content_type"] != "application/json" {
		t.Fatalf("content_type = %#v", env["content_type"])
	}
	if gotQuery != "limit=10&tags=cat&tags=dog" && gotQuery != "tags=cat&tags=dog&limit=10" {
		t.Fatalf("unexpected query: %q", gotQuery)
	}
}

func TestBuildHandler_UsesMergePatchContentType(t *testing.T) {
	var gotContentType string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer api.Close()

	ep := loadEndpoint(t, "/resources/{id}", "PATCH", "../../testdata/headers-and-auth.yaml")
	t.Setenv("MCP_AUTH_TOKEN", "secret")
	handler := newHandler(ep, Config{BaseURL: api.URL, ToolPrefix: "api", AuthProfile: "default"})
	_, err := handler(context.Background(), toolRequest(t, map[string]any{
		"path": map[string]any{"id": "res-1"},
		"body": map[string]any{"name": "updated"},
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if base, _, _ := mime.ParseMediaType(gotContentType); base != "application/merge-patch+json" {
		t.Fatalf("content type = %q", gotContentType)
	}
}

func TestBuildHandler_MultipartBinaryInput(t *testing.T) {
	var fileName, fileContent string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := r.MultipartReader()
		if err != nil {
			t.Fatalf("MultipartReader: %v", err)
		}
		part, err := reader.NextPart()
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		fileName = part.FileName()
		data, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("ReadAll(part): %v", err)
		}
		fileContent = string(data)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"uploaded":true}`))
	}))
	defer api.Close()

	ep := loadEndpoint(t, "/upload", "POST", "../../testdata/edge-cases.yaml")
	handler := newHandler(ep, Config{BaseURL: api.URL, ToolPrefix: "api", AuthProfile: "default"})
	_, err := handler(context.Background(), toolRequest(t, map[string]any{
		"body": map[string]any{
			"file": map[string]any{
				"source":       "base64",
				"data_base64":  base64.StdEncoding.EncodeToString([]byte("hello")),
				"filename":     "hello.txt",
				"content_type": "text/plain",
			},
		},
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if fileName != "hello.txt" || fileContent != "hello" {
		t.Fatalf("unexpected multipart payload: %q %q", fileName, fileContent)
	}
}

func TestBuildHandler_AuthResolutionAndErrorEnvelope(t *testing.T) {
	t.Run("bearer auth", func(t *testing.T) {
		var authz string
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authz = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer api.Close()

		t.Setenv("MCP_AUTH_TOKEN", "secret")
		ep := loadEndpoint(t, "/resources", "GET", "../../testdata/headers-and-auth.yaml")
		handler := newHandler(ep, Config{BaseURL: api.URL, ToolPrefix: "api", AuthProfile: "default"})
		result, err := handler(context.Background(), toolRequest(t, map[string]any{
			"headers": map[string]any{"X-Tenant": "acme"},
		}))
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if result.IsError {
			t.Fatalf("unexpected error: %#v", envelopeFromResult(t, result))
		}
		if authz != "Bearer secret" {
			t.Fatalf("authorization = %q", authz)
		}
	})

	t.Run("api error becomes MCP error envelope", func(t *testing.T) {
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad request"}`))
		}))
		defer api.Close()

		ep := loadEndpoint(t, "/error", "GET", "../../testdata/responses.yaml")
		handler := newHandler(ep, Config{BaseURL: api.URL, ToolPrefix: "api", AuthProfile: "default"})
		result, err := handler(context.Background(), toolRequest(t, map[string]any{}))
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if !result.IsError {
			t.Fatal("expected IsError=true for 400 response")
		}
		env := envelopeFromResult(t, result)
		if statusCode(t, env) != 400 {
			t.Fatalf("status = %#v", env["status"])
		}
	})
}

func TestBuildHandler_RejectsInsecureRemoteAuth(t *testing.T) {
	t.Setenv("MCP_AUTH_TOKEN", "secret")
	ep := spec.Endpoint{
		Method: "GET",
		Path:   "/items",
		SecurityRequirements: []spec.SecurityRequirement{
			{Schemes: []spec.SecurityInfo{{Name: "bearerAuth", Type: "http", Scheme: "bearer"}}},
		},
		Responses: []spec.ResponseInfo{{StatusCode: "200"}},
	}
	handler := newHandler(ep, Config{
		BaseURL:           "http://example.com",
		ToolPrefix:        "api",
		AuthProfile:       "default",
		AllowInsecureHTTP: false,
	})
	result, err := handler(context.Background(), toolRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected insecure transport error")
	}
	env := envelopeFromResult(t, result)
	if statusCode(t, env) != 0 {
		t.Fatalf("status = %#v", env["status"])
	}
	proxyErr := env["proxy_error"].(map[string]any)
	if proxyErr["code"] != "insecure_transport" {
		t.Fatalf("proxy_error = %#v", proxyErr)
	}
}

func TestBuildHandler_EmitsImageContentForBinaryResponse(t *testing.T) {
	pngBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01, 0x02, 0x03}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	defer api.Close()

	ep := spec.Endpoint{
		Method:    "GET",
		Path:      "/icon",
		Responses: []spec.ResponseInfo{{StatusCode: "200", Content: []spec.MediaType{{ContentType: "image/png"}}}},
	}
	handler := newHandler(ep, Config{BaseURL: api.URL, ToolPrefix: "api", AuthProfile: "default"})
	result, err := handler(context.Background(), toolRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %#v", envelopeFromResult(t, result))
	}
	if len(result.Content) != 2 {
		t.Fatalf("expected 2 content blocks (text + image), got %d", len(result.Content))
	}
	img, ok := result.Content[1].(*mcp.ImageContent)
	if !ok {
		t.Fatalf("expected ImageContent at index 1, got %T", result.Content[1])
	}
	if img.MIMEType != "image/png" {
		t.Fatalf("MIMEType = %q", img.MIMEType)
	}
	if !bytes.Equal(img.Data, pngBytes) {
		t.Fatalf("image bytes mismatch: got %x", img.Data)
	}
	env := envelopeFromResult(t, result)
	if env["content_type"] != "image/png" {
		t.Fatalf("envelope content_type = %#v", env["content_type"])
	}
}

func TestBuildHandler_EmitsAudioContentForBinaryResponse(t *testing.T) {
	audio := []byte{0x49, 0x44, 0x33, 0x04, 0x00, 0x00, 0x00}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(audio)
	}))
	defer api.Close()

	ep := spec.Endpoint{
		Method:    "GET",
		Path:      "/clip",
		Responses: []spec.ResponseInfo{{StatusCode: "200", Content: []spec.MediaType{{ContentType: "audio/mpeg"}}}},
	}
	handler := newHandler(ep, Config{BaseURL: api.URL, ToolPrefix: "api", AuthProfile: "default"})
	result, err := handler(context.Background(), toolRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if len(result.Content) != 2 {
		t.Fatalf("expected 2 content blocks (text + audio), got %d", len(result.Content))
	}
	got, ok := result.Content[1].(*mcp.AudioContent)
	if !ok {
		t.Fatalf("expected AudioContent at index 1, got %T", result.Content[1])
	}
	if got.MIMEType != "audio/mpeg" {
		t.Fatalf("MIMEType = %q", got.MIMEType)
	}
	if !bytes.Equal(got.Data, audio) {
		t.Fatalf("audio bytes mismatch: got %x", got.Data)
	}
}

func TestBuildHandler_SVGImageEmitsImageContent(t *testing.T) {
	svg := `<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"></svg>`
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = io.WriteString(w, svg)
	}))
	defer api.Close()

	ep := spec.Endpoint{
		Method:    "GET",
		Path:      "/icon.svg",
		Responses: []spec.ResponseInfo{{StatusCode: "200", Content: []spec.MediaType{{ContentType: "image/svg+xml"}}}},
	}
	handler := newHandler(ep, Config{BaseURL: api.URL, ToolPrefix: "api", AuthProfile: "default"})
	result, err := handler(context.Background(), toolRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if len(result.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(result.Content))
	}
	img, ok := result.Content[1].(*mcp.ImageContent)
	if !ok {
		t.Fatalf("expected ImageContent at index 1, got %T", result.Content[1])
	}
	if string(img.Data) != svg {
		t.Fatalf("svg bytes mismatch: %q", string(img.Data))
	}
}

func TestBuildHandler_NonMediaResponseHasNoExtraContent(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer api.Close()

	ep := loadEndpoint(t, "/pets", "GET", "../../testdata/petstore.yaml")
	handler := newHandler(ep, Config{BaseURL: api.URL, ToolPrefix: "api", AuthProfile: "default"})
	result, err := handler(context.Background(), toolRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("expected single text content block, got %d", len(result.Content))
	}
	if _, ok := result.Content[0].(*mcp.TextContent); !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
}

func TestSerializeRequestBody_PathSourceReadsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blob.txt")
	if err := os.WriteFile(path, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	data, ct, err := serializeRequestBody(&spec.RequestBody{
		Required: true,
		Content:  []spec.MediaType{{ContentType: "application/octet-stream", Schema: map[string]any{"type": "string", "format": "binary"}}},
	}, map[string]any{"source": "path", "path": path, "content_type": "text/plain"})
	if err != nil {
		t.Fatalf("serializeRequestBody: %v", err)
	}
	if string(data) != "hello" || ct != "application/octet-stream" {
		t.Fatalf("unexpected serialized body: %q %q", string(data), ct)
	}
}

func TestMultipartWriterPreservesExplicitPartContentType(t *testing.T) {
	var partCT string
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	err := writeMultipartPart(writer, "file", map[string]any{
		"source":       "base64",
		"data_base64":  base64.StdEncoding.EncodeToString([]byte("x")),
		"filename":     "x.txt",
		"content_type": "text/plain",
	}, spec.Encoding{})
	if err != nil {
		t.Fatalf("writeMultipartPart: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader := multipart.NewReader(&body, writer.Boundary())
	part, err := reader.NextPart()
	if err != nil {
		t.Fatalf("NextPart: %v", err)
	}
	partCT = part.Header.Get("Content-Type")
	if partCT != "text/plain" {
		t.Fatalf("part content type = %q", partCT)
	}
}

func TestRedactSecretValue_RedactsSecretsInsideJSONString(t *testing.T) {
	input := map[string]any{
		"addition": `{"root_folder_id":"root","refresh_token":"refresh-secret","AccessToken":"access-secret","cookie":"session=secret","safe":"keep"}`,
	}
	redacted := redactResponseEnvelope(input)
	raw, ok := redacted["addition"].(string)
	if !ok {
		t.Fatalf("addition type = %T, want string", redacted["addition"])
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("redacted addition is not valid JSON: %v", err)
	}
	for _, key := range []string{"refresh_token", "AccessToken", "cookie"} {
		if decoded[key] != "[REDACTED]" {
			t.Fatalf("%s = %#v, want [REDACTED]", key, decoded[key])
		}
	}
	if decoded["safe"] != "keep" {
		t.Fatalf("safe = %#v, want keep", decoded["safe"])
	}
}
