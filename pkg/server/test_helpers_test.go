package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rendis/mcp-openapi-proxy/pkg/spec"
)

func toolRequest(t *testing.T, args map[string]any) *mcp.CallToolRequest {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Arguments: data},
	}
}

func envelopeFromResult(t *testing.T, result *mcp.CallToolResult) map[string]any {
	t.Helper()
	env, ok := result.StructuredContent.(map[string]any)
	if ok {
		return env
	}
	text := result.Content[0].(*mcp.TextContent).Text
	var parsed map[string]any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	return parsed
}

func listToolsResult(t *testing.T, session *mcp.ClientSession) *mcp.ListToolsResult {
	t.Helper()
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	return res
}

func statusCode(t *testing.T, env map[string]any) int {
	t.Helper()
	switch v := env["status"].(type) {
	case int:
		return v
	case float64:
		return int(v)
	default:
		t.Fatalf("unexpected status type %T", env["status"])
		return 0
	}
}

func loadEndpoints(t *testing.T, specPath string) []spec.Endpoint {
	t.Helper()
	eps, _, err := spec.LoadSpec(specPath, true)
	if err != nil {
		t.Fatalf("LoadSpec(%s): %v", specPath, err)
	}
	return eps
}

func loadEndpoint(t *testing.T, path, method, specPath string) spec.Endpoint {
	t.Helper()
	for _, ep := range loadEndpoints(t, specPath) {
		if ep.Path == path && ep.Method == method {
			return ep
		}
	}
	t.Fatalf("endpoint %s %s not found", method, path)
	return spec.Endpoint{}
}

func newClientSession(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		_ = serverSession.Close()
		t.Fatalf("client.Connect: %v", err)
	}

	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Close()
	})
	return clientSession
}

func listToolNames(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()
	res := listToolsResult(t, session)
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func callToolViaSession(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res
}

func requireToolNamesContain(t *testing.T, names []string, want ...string) {
	t.Helper()
	for _, item := range want {
		found := false
		for _, name := range names {
			if name == item {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("tool %q not found in %v", item, names)
		}
	}
}

func requireToolNamesOmit(t *testing.T, names []string, unwanted ...string) {
	t.Helper()
	for _, item := range unwanted {
		for _, name := range names {
			if name == item {
				t.Fatalf("tool %q unexpectedly present in %v", item, names)
			}
		}
	}
}
