package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestGenerateMultiAppTools_CountDoesNotScaleWithApps(t *testing.T) {
	for _, count := range []int{1, 6, 20} {
		t.Run(fmt.Sprintf("apps_%d", count), func(t *testing.T) {
			runtime := &multiAppRuntime{apps: map[string]*appRuntime{}}
			for i := 0; i < count; i++ {
				name := fmt.Sprintf("app%d", i)
				runtime.apps[name] = &appRuntime{name: name, catalog: &endpointCatalog{byToolName: map[string]endpointEntry{}}}
			}
			srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
			GenerateMultiAppTools(srv, runtime)
			names := listToolNames(t, newClientSession(t, srv))
			if len(names) != 2 {
				t.Fatalf("tool count = %d, want 2; names=%v", len(names), names)
			}
			requireToolNamesContain(t, names, multiDiscoverToolName, multiCallToolName)
		})
	}
}

func TestMultiApp_DiscoverCallCredentialIsolationAndRedaction(t *testing.T) {
	var alphaKey, betaKey string
	alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alphaKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"app":"alpha","apiKey":"should-not-leak","nested":{"token":"also-secret"}}`))
	}))
	defer alpha.Close()
	beta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		betaKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"app":"beta","clientSecret":"should-not-leak"}`))
	}))
	defer beta.Close()

	t.Setenv("ALPHA_API_KEY", "alpha-test-secret")
	t.Setenv("BETA_API_KEY", "beta-test-secret")

	dir := t.TempDir()
	specBody := `openapi: "3.0.3"
info:
  title: Test API
  version: "1"
components:
  securitySchemes:
    X-Api-Key:
      type: apiKey
      in: header
      name: X-Api-Key
security:
  - X-Api-Key: []
paths:
  /whoami:
    get:
      summary: Who am I
      operationId: whoami
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: object
`
	for _, name := range []string{"alpha", "beta"} {
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(specBody), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := AppsConfig{Apps: map[string]AppDefinition{
		"alpha": {Spec: "alpha.yaml", BaseURL: alpha.URL, AllowInsecureHTTP: true, Credentials: map[string]AppCredentialEnvRef{"X-Api-Key": {KeyEnv: "ALPHA_API_KEY"}}},
		"beta":  {Spec: "beta.yaml", BaseURL: beta.URL, AllowInsecureHTTP: true, Credentials: map[string]AppCredentialEnvRef{"X-Api-Key": {KeyEnv: "BETA_API_KEY"}}},
	}}
	cfgBytes, _ := json.Marshal(cfg)
	cfgPath := filepath.Join(dir, "apps.json")
	if err := os.WriteFile(cfgPath, cfgBytes, 0600); err != nil {
		t.Fatal(err)
	}

	runtime, err := loadMultiAppRuntime(cfgPath, nil, 1<<20)
	if err != nil {
		t.Fatalf("loadMultiAppRuntime: %v", err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	GenerateMultiAppTools(srv, runtime)
	session := newClientSession(t, srv)

	apps := envelopeFromResult(t, callToolViaSession(t, session, multiDiscoverToolName, map[string]any{}))
	if got := len(apps["apps"].([]any)); got != 2 {
		t.Fatalf("apps count = %d, want 2", got)
	}

	listed := envelopeFromResult(t, callToolViaSession(t, session, multiDiscoverToolName, map[string]any{"app": "alpha", "q": "who"}))
	items := listed["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("discover items = %d, want 1", len(items))
	}
	operation := items[0].(map[string]any)["operation"].(string)
	if operation != "alpha_get_whoami" {
		t.Fatalf("operation = %q", operation)
	}

	described := envelopeFromResult(t, callToolViaSession(t, session, multiDiscoverToolName, map[string]any{"app": "alpha", "operation": operation}))
	if described["app"] != "alpha" || described["path"] != "/whoami" {
		t.Fatalf("unexpected describe payload: %#v", described)
	}

	alphaResult := envelopeFromResult(t, callToolViaSession(t, session, multiCallToolName, map[string]any{"app": "alpha", "operation": operation}))
	if alphaKey != "alpha-test-secret" {
		t.Fatalf("alpha credential isolation failed: got header %q", alphaKey)
	}
	alphaBody := alphaResult["body"].(map[string]any)
	if alphaBody["apiKey"] != "[REDACTED]" || alphaBody["nested"].(map[string]any)["token"] != "[REDACTED]" {
		t.Fatalf("alpha response was not recursively redacted: %#v", alphaBody)
	}

	betaResult := envelopeFromResult(t, callToolViaSession(t, session, multiCallToolName, map[string]any{"app": "beta", "operation": "beta_get_whoami"}))
	if betaKey != "beta-test-secret" {
		t.Fatalf("beta credential isolation failed: got header %q", betaKey)
	}
	betaBody := betaResult["body"].(map[string]any)
	if betaBody["clientSecret"] != "[REDACTED]" {
		t.Fatalf("beta response was not redacted: %#v", betaBody)
	}
}

func TestMultiApp_ConfigAndUnknownAppFailuresAreClean(t *testing.T) {
	dir := t.TempDir()
	badPath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badPath, []byte(`{"apps":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMultiAppRuntime(badPath, nil, 1024); err == nil {
		t.Fatal("empty apps config unexpectedly succeeded")
	}

	runtime := &multiAppRuntime{apps: map[string]*appRuntime{}}
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	GenerateMultiAppTools(srv, runtime)
	res := callToolViaSession(t, newClientSession(t, srv), multiDiscoverToolName, map[string]any{"app": "missing"})
	if !res.IsError {
		t.Fatal("unknown app should return an MCP error result")
	}
	env := envelopeFromResult(t, res)
	encoded, _ := json.Marshal(env)
	if string(encoded) == "" {
		t.Fatal("empty error envelope")
	}
}
