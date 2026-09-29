package spec

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSpecSwagger2JSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "swagger.json")
	body := `{
  "swagger": "2.0",
  "info": {"title": "Legacy API", "version": "1.0"},
  "host": "example.test",
  "basePath": "/api",
  "schemes": ["https"],
  "securityDefinitions": {
    "apikey": {"type":"apiKey", "in":"header", "name":"X-API-KEY"}
  },
  "security": [{"apikey":[]}],
  "paths": {
    "/status": {
      "get": {
        "responses": {"200": {"description":"ok", "schema":{"type":"object"}}}
      }
    }
  }
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	eps, doc, err := LoadSpec(path, true)
	if err != nil {
		t.Fatalf("LoadSpec Swagger2: %v", err)
	}
	if doc == nil || len(eps) != 1 {
		t.Fatalf("doc=%v endpoints=%d, want 1", doc != nil, len(eps))
	}
	if eps[0].Path != "/status" || eps[0].Method != "GET" {
		t.Fatalf("unexpected endpoint: %#v", eps[0])
	}
	if len(eps[0].SecurityRequirements) != 1 || len(eps[0].SecurityRequirements[0].Schemes) != 1 {
		t.Fatalf("security requirements not preserved: %#v", eps[0].SecurityRequirements)
	}
	if eps[0].SecurityRequirements[0].Schemes[0].Name != "apikey" {
		t.Fatalf("security scheme=%q, want apikey", eps[0].SecurityRequirements[0].Schemes[0].Name)
	}
}
