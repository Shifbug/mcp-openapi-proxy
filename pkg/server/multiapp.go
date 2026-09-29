package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rendis/mcp-openapi-proxy/pkg/auth"
	"github.com/rendis/mcp-openapi-proxy/pkg/client"
	"github.com/rendis/mcp-openapi-proxy/pkg/spec"
)

const (
	multiDiscoverToolName = "container_api_discover"
	multiCallToolName     = "container_api_call"
)

// AppsConfig is the on-disk configuration for multi-app mode.
type AppsConfig struct {
	Apps map[string]AppDefinition `json:"apps"`
}

// AppDefinition describes one OpenAPI-backed application without containing
// secret values. Credential fields are environment-variable references only.
type AppDefinition struct {
	Spec               string                         `json:"spec"`
	BaseURL            string                         `json:"base_url,omitempty"`
	BaseURLEnv         string                         `json:"base_url_env,omitempty"`
	SkipSpecValidation bool                           `json:"skip_spec_validation,omitempty"`
	ExcludeDeprecated  bool                           `json:"exclude_deprecated,omitempty"`
	AllowInsecureHTTP  bool                           `json:"allow_insecure_http,omitempty"`
	Credentials        map[string]AppCredentialEnvRef `json:"credentials,omitempty"`
}

// AppCredentialEnvRef maps one OpenAPI security scheme to environment-variable
// names. No credential values are stored in AppsConfig.
type AppCredentialEnvRef struct {
	KeyEnv      string `json:"key_env,omitempty"`
	TokenEnv    string `json:"token_env,omitempty"`
	UsernameEnv string `json:"username_env,omitempty"`
	PasswordEnv string `json:"password_env,omitempty"`
}

type appRuntime struct {
	name         string
	catalog      *endpointCatalog
	cfg          Config
	httpClient   *client.Client
	authResolver *auth.Resolver
}

type multiAppRuntime struct {
	apps map[string]*appRuntime
}

// RunMultiApp loads multiple OpenAPI specs and exposes a fixed two-tool MCP
// surface regardless of application count.
func RunMultiApp(configPath string, extraHeaders map[string]string, maxBodyBytes int64) error {
	runtime, err := loadMultiAppRuntime(configPath, extraHeaders, maxBodyBytes)
	if err != nil {
		return err
	}

	srv := mcp.NewServer(
		&mcp.Implementation{Name: "mcp-openapi-proxy", Version: "0.1.0"},
		nil,
	)
	GenerateMultiAppTools(srv, runtime)

	endpointCount := 0
	for _, app := range runtime.apps {
		endpointCount += app.catalog.count()
	}
	fmt.Fprintf(os.Stderr, "mcp-openapi-proxy: registered 2 tools for %d apps and %d indexed endpoints from %s\n", len(runtime.apps), endpointCount, configPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Printf("MCP server error: %v", err)
		return err
	}
	return nil
}

func loadMultiAppRuntime(configPath string, extraHeaders map[string]string, maxBodyBytes int64) (*multiAppRuntime, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read MCP_APPS_CONFIG: %w", err)
	}
	var cfg AppsConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse MCP_APPS_CONFIG: %w", err)
	}
	if len(cfg.Apps) == 0 {
		return nil, fmt.Errorf("MCP_APPS_CONFIG must contain at least one app")
	}

	baseDir := filepath.Dir(configPath)
	runtime := &multiAppRuntime{apps: make(map[string]*appRuntime, len(cfg.Apps))}
	for rawName, def := range cfg.Apps {
		name := strings.TrimSpace(rawName)
		if name == "" {
			return nil, fmt.Errorf("MCP_APPS_CONFIG contains an empty app name")
		}
		if name != rawName {
			return nil, fmt.Errorf("app name %q must not have surrounding whitespace", rawName)
		}
		if strings.TrimSpace(def.Spec) == "" {
			return nil, fmt.Errorf("app %q: spec is required", name)
		}
		if def.BaseURL != "" && def.BaseURLEnv != "" {
			return nil, fmt.Errorf("app %q: base_url and base_url_env are mutually exclusive", name)
		}

		specSource := def.Spec
		if !isRemoteSpecSource(specSource) && !filepath.IsAbs(specSource) {
			specSource = filepath.Join(baseDir, specSource)
		}
		endpoints, _, err := spec.LoadSpec(specSource, !def.SkipSpecValidation)
		if err != nil {
			return nil, fmt.Errorf("app %q: load spec: %w", name, err)
		}

		baseURL := strings.TrimRight(strings.TrimSpace(def.BaseURL), "/")
		if def.BaseURLEnv != "" {
			baseURL = strings.TrimRight(strings.TrimSpace(os.Getenv(def.BaseURLEnv)), "/")
			if baseURL == "" {
				return nil, fmt.Errorf("app %q: configured base URL environment variable is empty", name)
			}
		}

		credentialRefs := make(map[string]auth.CredentialEnvRefs, len(def.Credentials))
		for scheme, ref := range def.Credentials {
			if strings.TrimSpace(scheme) == "" {
				return nil, fmt.Errorf("app %q: credential scheme name cannot be empty", name)
			}
			credentialRefs[scheme] = auth.CredentialEnvRefs{
				KeyEnv:      strings.TrimSpace(ref.KeyEnv),
				TokenEnv:    strings.TrimSpace(ref.TokenEnv),
				UsernameEnv: strings.TrimSpace(ref.UsernameEnv),
				PasswordEnv: strings.TrimSpace(ref.PasswordEnv),
			}
		}

		appCfg := Config{
			SpecSource:         specSource,
			BaseURL:            baseURL,
			ToolPrefix:         name,
			ExcludeDeprecated:  def.ExcludeDeprecated,
			AllowInsecureHTTP:  def.AllowInsecureHTTP,
			MaxBodyBytes:       maxBodyBytes,
			AuthProfile:        name,
			SkipSpecValidation: def.SkipSpecValidation,
		}
		runtime.apps[name] = &appRuntime{
			name:         name,
			catalog:      newEndpointCatalog(endpoints, name, def.ExcludeDeprecated),
			cfg:          appCfg,
			httpClient:   client.New(extraHeaders, maxBodyBytes),
			authResolver: auth.NewResolverWithCredentialEnv(name, credentialRefs),
		}
	}
	return runtime, nil
}

func isRemoteSpecSource(source string) bool {
	s := strings.ToLower(strings.TrimSpace(source))
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// GenerateMultiAppTools registers exactly two tools regardless of app count.
func GenerateMultiAppTools(srv *mcp.Server, runtime *multiAppRuntime) {
	srv.AddTool(buildMultiDiscoverTool(), buildMultiDiscoverHandler(runtime))
	srv.AddTool(buildMultiCallTool(), buildMultiCallHandler(runtime))
}

func buildMultiDiscoverTool() *mcp.Tool {
	return &mcp.Tool{
		Name:        multiDiscoverToolName,
		Description: "Discover configured container APIs, search endpoints, or describe one endpoint contract.",
		InputSchema: buildMultiDiscoverInputSchema(),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}
}

func buildMultiCallTool() *mcp.Tool {
	return &mcp.Tool{
		Name:        multiCallToolName,
		Description: "Execute one endpoint against a selected container API using server-side credentials.",
		InputSchema: buildMultiCallInputSchema(),
	}
}

func buildMultiDiscoverInputSchema() *jsonschema.Schema {
	return mapToJSONSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"app":         map[string]any{"type": "string"},
			"operation":   map[string]any{"type": "string"},
			"toolName":    map[string]any{"type": "string"},
			"q":           map[string]any{"type": "string"},
			"tag":         map[string]any{"type": "string"},
			"path_prefix": map[string]any{"type": "string"},
			"method":      map[string]any{"type": "string"},
			"auth":        map[string]any{"type": "string"},
			"deprecated":  map[string]any{"type": "boolean"},
			"cursor":      map[string]any{"type": "string"},
			"limit":       map[string]any{"type": "integer", "minimum": 1, "maximum": maxEndpointListLimit},
		},
		"additionalProperties": false,
	})
}

func buildMultiCallInputSchema() *jsonschema.Schema {
	return mapToJSONSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"app":       map[string]any{"type": "string"},
			"operation": map[string]any{"type": "string"},
			"toolName":  map[string]any{"type": "string"},
			"path":      genericArgumentSectionSchema(),
			"query":     genericArgumentSectionSchema(),
			"headers":   genericArgumentSectionSchema(),
			"cookies":   genericArgumentSectionSchema(),
			"body":      genericArgumentSectionSchema(),
		},
		"required":             []string{"app"},
		"additionalProperties": false,
	})
}

func buildMultiDiscoverHandler(runtime *multiAppRuntime) mcp.ToolHandler {
	return func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArguments(req)
		if err != nil {
			return errorResult("invalid_arguments", err.Error(), nil), nil
		}
		appName, err := optionalStringArg(args, "app")
		if err != nil {
			return errorResult("invalid_arguments", err.Error(), nil), nil
		}
		operation, err := multiOperationArg(args)
		if err != nil {
			return errorResult("invalid_arguments", err.Error(), nil), nil
		}
		if appName == "" {
			if operation != "" {
				return errorResult("invalid_arguments", "app is required when operation is provided", nil), nil
			}
			names := make([]string, 0, len(runtime.apps))
			for name := range runtime.apps {
				names = append(names, name)
			}
			sort.Strings(names)
			items := make([]any, 0, len(names))
			for _, name := range names {
				items = append(items, map[string]any{
					"app":           name,
					"endpointCount": runtime.apps[name].catalog.count(),
				})
			}
			return toolResult(map[string]any{"apps": items}, false), nil
		}

		app, ok := runtime.apps[appName]
		if !ok {
			return errorResult("unknown_app", fmt.Sprintf("app %q is not configured", appName), nil), nil
		}
		if operation != "" {
			entry, ok := app.catalog.byToolName[operation]
			if !ok {
				return errorResult("unknown_operation", fmt.Sprintf("operation %q is not indexed for app %q", operation, appName), nil), nil
			}
			payload := describeEndpointPayload(entry, app.cfg)
			payload["app"] = appName
			return toolResult(payload, false), nil
		}

		filter, err := parseEndpointListFilter(args)
		if err != nil {
			return errorResult("invalid_arguments", err.Error(), nil), nil
		}
		filtered := filterEndpointEntries(app.catalog.entries, filter)
		start, err := decodeListCursor(filter.Cursor)
		if err != nil {
			return errorResult("invalid_cursor", err.Error(), nil), nil
		}
		if start < 0 || start > len(filtered) {
			return errorResult("invalid_cursor", "cursor is out of range", map[string]any{"cursor": filter.Cursor}), nil
		}
		end := start + filter.Limit
		if end > len(filtered) {
			end = len(filtered)
		}
		nextCursor := ""
		if end < len(filtered) {
			nextCursor = encodeListCursor(end)
		}
		items := make([]any, 0, end-start)
		for _, entry := range filtered[start:end] {
			items = append(items, map[string]any{
				"operation":    entry.ToolName,
				"method":       entry.Endpoint.Method,
				"path":         entry.Endpoint.Path,
				"description":  entry.Description,
				"requiredAuth": entry.RequiredAuth,
				"tags":         cloneStrings(entry.Endpoint.Tags),
				"deprecated":   entry.Endpoint.Deprecated,
			})
		}
		return toolResult(map[string]any{
			"app":        appName,
			"items":      items,
			"nextCursor": nextCursor,
		}, false), nil
	}
}

func buildMultiCallHandler(runtime *multiAppRuntime) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArguments(req)
		if err != nil {
			return errorResult("invalid_arguments", err.Error(), nil), nil
		}
		appName, err := requiredStringArg(args, "app")
		if err != nil {
			return errorResult("invalid_arguments", err.Error(), nil), nil
		}
		operation, err := multiOperationArg(args)
		if err != nil {
			return errorResult("invalid_arguments", err.Error(), nil), nil
		}
		if operation == "" {
			return errorResult("invalid_arguments", "operation is required", nil), nil
		}
		app, ok := runtime.apps[appName]
		if !ok {
			return errorResult("unknown_app", fmt.Sprintf("app %q is not configured", appName), nil), nil
		}
		entry, ok := app.catalog.byToolName[operation]
		if !ok {
			return errorResult("unknown_operation", fmt.Sprintf("operation %q is not indexed for app %q", operation, appName), nil), nil
		}
		return callEndpointArgs(ctx, entry.Endpoint, app.httpClient, app.authResolver, app.cfg, args)
	}
}

func multiOperationArg(args map[string]any) (string, error) {
	operation, err := optionalStringArg(args, "operation")
	if err != nil {
		return "", err
	}
	legacy, err := optionalStringArg(args, "toolName")
	if err != nil {
		return "", err
	}
	if operation != "" && legacy != "" && operation != legacy {
		return "", fmt.Errorf("operation and toolName disagree")
	}
	if operation != "" {
		return operation, nil
	}
	return legacy, nil
}
