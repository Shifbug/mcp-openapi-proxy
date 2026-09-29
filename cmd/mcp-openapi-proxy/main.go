package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/rendis/mcp-openapi-proxy/pkg/auth"
	"github.com/rendis/mcp-openapi-proxy/pkg/server"
	"github.com/rendis/mcp-openapi-proxy/pkg/spec"
)

const tokenPrefix = "default"

var (
	discoverOIDCEndpoints = auth.DiscoverOIDCEndpoints
	runAuthLogin          = auth.RunLogin
	loadSpec              = spec.LoadSpec
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	var err error
	switch cmd {
	case "serve", "":
		err = runServe()
	case "login":
		err = runLogin(os.Args[2:]...)
	case "logout":
		err = runLogout()
	case "status":
		err = runStatus()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		fmt.Fprintf(os.Stderr, "usage: mcp-openapi-proxy [serve|login|logout|status]\n")
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func runServe() error {
	extraHeaders := parseExtraHeaders(os.Getenv("MCP_EXTRA_HEADERS"))
	maxBodyBytes, err := parseInt64Env("MCP_MAX_BODY_BYTES", 10<<20)
	if err != nil {
		return err
	}

	if appsConfig := strings.TrimSpace(os.Getenv("MCP_APPS_CONFIG")); appsConfig != "" {
		return server.RunMultiApp(appsConfig, extraHeaders, maxBodyBytes)
	}

	specSource := os.Getenv("MCP_SPEC")
	if specSource == "" {
		return fmt.Errorf("MCP_SPEC environment variable is required unless MCP_APPS_CONFIG is set")
	}

	toolPrefix := os.Getenv("MCP_TOOL_PREFIX")
	if toolPrefix == "" {
		toolPrefix = "api"
	}

	cfg := server.Config{
		SpecSource:         specSource,
		BaseURL:            strings.TrimRight(os.Getenv("MCP_BASE_URL"), "/"),
		ToolPrefix:         toolPrefix,
		ExcludeDeprecated:  parseBoolEnv("MCP_EXCLUDE_DEPRECATED"),
		AllowInsecureHTTP:  parseBoolEnv("MCP_ALLOW_INSECURE_HTTP"),
		MaxBodyBytes:       maxBodyBytes,
		AuthProfile:        resolveAuthProfile(toolPrefix),
		SkipSpecValidation: parseBoolEnv("MCP_SKIP_SPEC_VALIDATION"),
	}

	return server.Run(cfg, extraHeaders)
}

func runLogin(args ...string) error {
	loginArgs, err := parseLoginArgs(args)
	if err != nil {
		return err
	}

	loginArgs, err = completeLoginArgs(loginArgs, currentLoginEnv(os.Getenv))
	if err != nil {
		return err
	}

	env, err := resolveLoginEnv(loginArgs)
	if err != nil {
		return err
	}
	return runLoginWithEnv(env)
}

func runLoginWithEnv(env map[string]string) error {
	cfg := auth.LoginConfig{
		TokenPrefix: resolveAuthProfileValues(env["MCP_AUTH_PROFILE"], env["MCP_TOOL_PREFIX"]),
		Scopes:      resolveOIDCScopesFromEnv(env),
	}

	issuer := strings.TrimSpace(env["MCP_OIDC_ISSUER"])
	clientID := strings.TrimSpace(env["MCP_OIDC_CLIENT_ID"])

	if issuer != "" && clientID != "" {
		// Discover endpoints via .well-known/openid-configuration (standard OIDC).
		authEP, tokenEP, err := discoverOIDCEndpoints(issuer)
		if err != nil {
			return fmt.Errorf("OIDC discovery from %s: %w", issuer, err)
		}
		cfg.AuthEndpoint = authEP
		cfg.TokenEndpoint = tokenEP
		cfg.ClientID = clientID
	} else if baseURL := strings.TrimRight(strings.TrimSpace(env["MCP_BASE_URL"]), "/"); baseURL != "" {
		// Fetch auth config from API.
		cfg.APIBaseURL = baseURL
	} else {
		return fmt.Errorf("login requires MCP_OIDC_ISSUER + MCP_OIDC_CLIENT_ID, or MCP_BASE_URL")
	}

	return runAuthLogin(cfg)
}

func hasSufficientLoginEnv(env map[string]string) bool {
	issuer := strings.TrimSpace(env["MCP_OIDC_ISSUER"])
	clientID := strings.TrimSpace(env["MCP_OIDC_CLIENT_ID"])
	if issuer != "" && clientID != "" {
		return true
	}
	return strings.TrimSpace(env["MCP_BASE_URL"]) != ""
}

func runLogout() error {
	return auth.RunLogout(resolveAuthProfile(os.Getenv("MCP_TOOL_PREFIX")))
}

func runStatus() error {
	return auth.RunStatus(resolveAuthProfile(os.Getenv("MCP_TOOL_PREFIX")))
}

// parseExtraHeaders parses a comma-separated list of key:value pairs.
func parseExtraHeaders(raw string) map[string]string {
	headers := make(map[string]string)
	if raw == "" {
		return headers
	}

	pairs := strings.Split(raw, ",")
	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, ":", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			if key != "" {
				headers[key] = value
			}
		}
	}

	return headers
}

// resolveTokenProvider is kept as a compatibility helper for tests. Runtime
// auth is resolved per-endpoint in the server package.
func resolveTokenProvider() auth.TokenProvider {
	if token := strings.TrimSpace(os.Getenv("MCP_AUTH_TOKEN")); token != "" {
		return auth.NewStaticTokenProvider(token)
	}
	profile := resolveAuthProfile(os.Getenv("MCP_TOOL_PREFIX"))
	tp, err := auth.NewOIDCTokenProvider(auth.TokenFilePath(profile))
	if err == nil {
		return tp
	}
	fmt.Fprintf(os.Stderr, "warning: no auth token configured (set MCP_AUTH_TOKEN or run login)\n")
	return auth.NewStaticTokenProvider("")
}

func resolveAuthProfile(toolPrefix string) string {
	return resolveAuthProfileValues(os.Getenv("MCP_AUTH_PROFILE"), toolPrefix)
}

func resolveAuthProfileValues(authProfile, toolPrefix string) string {
	if profile := strings.TrimSpace(authProfile); profile != "" {
		return profile
	}
	if toolPrefix = strings.TrimSpace(toolPrefix); toolPrefix != "" {
		return toolPrefix
	}
	return "default"
}

func resolveOIDCScopes() string {
	return resolveOIDCScopesFromEnv(currentLoginEnv(os.Getenv))
}

func resolveOIDCScopesFromEnv(env map[string]string) string {
	if scopes := strings.TrimSpace(env["MCP_OIDC_SCOPES"]); scopes != "" {
		return scopes
	}
	specSource := strings.TrimSpace(env["MCP_SPEC"])
	if specSource == "" {
		return ""
	}
	_, doc, err := loadSpec(specSource, true)
	if err != nil || doc == nil {
		return ""
	}
	collectedScopes := spec.CollectOAuthScopes(doc)
	if len(collectedScopes) == 0 {
		return ""
	}
	return strings.Join(collectedScopes, " ")
}

func parseBoolEnv(name string) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func parseInt64Env(name string, defaultValue int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return defaultValue, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return n, nil
}
