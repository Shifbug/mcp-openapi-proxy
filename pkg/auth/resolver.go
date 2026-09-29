package auth

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/rendis/mcp-openapi-proxy/pkg/spec"
)

// AppliedAuth is the concrete authentication material to add to an HTTP request.
type AppliedAuth struct {
	Headers http.Header
	Query   url.Values
	Cookies map[string]string
}

// Resolver resolves OpenAPI security requirements against environment variables
// and the local OIDC token cache.
type Resolver struct {
	profile       string
	globalBearer  string
	credentialEnv map[string]CredentialEnvRefs
	oidc          TokenProvider
	oidcErr       error
	oidcLoaded    bool
}

// NewResolver creates a resolver scoped to an auth profile.
func NewResolver(profile string) *Resolver {
	return &Resolver{
		profile:      normalizeProfile(profile),
		globalBearer: strings.TrimSpace(os.Getenv("MCP_AUTH_TOKEN")),
	}
}

// Resolve picks the first satisfiable security requirement. If the endpoint is
// public or explicitly allows anonymous access, it returns an empty auth set.
func (r *Resolver) Resolve(ctx context.Context, requirements []spec.SecurityRequirement) (*AppliedAuth, error) {
	if len(requirements) == 0 {
		return &AppliedAuth{}, nil
	}

	var errs []string
	for _, req := range requirements {
		if len(req.Schemes) == 0 {
			return &AppliedAuth{}, nil
		}

		applied := &AppliedAuth{
			Headers: http.Header{},
			Query:   url.Values{},
			Cookies: map[string]string{},
		}

		var failed bool
		for _, scheme := range req.Schemes {
			if err := r.applyScheme(ctx, applied, scheme); err != nil {
				errs = append(errs, err.Error())
				failed = true
				break
			}
		}
		if !failed {
			return applied, nil
		}
	}

	if len(errs) == 0 {
		return nil, fmt.Errorf("authentication required but no usable credentials are configured")
	}

	sort.Strings(errs)
	return nil, fmt.Errorf("authentication required but not configured: %s", strings.Join(dedupeStrings(errs), "; "))
}

func (r *Resolver) applyScheme(ctx context.Context, applied *AppliedAuth, scheme spec.SecurityInfo) error {
	switch {
	case scheme.Type == "http" && strings.EqualFold(scheme.Scheme, "bearer"):
		token, err := r.resolveBearerToken(ctx, scheme.Name, true)
		if err != nil {
			return err
		}
		applied.Headers.Set("Authorization", "Bearer "+token)
		return nil

	case scheme.Type == "http" && strings.EqualFold(scheme.Scheme, "basic"):
		username := r.envValue(scheme.Name, "USERNAME")
		password := r.envValue(scheme.Name, "PASSWORD")
		if username == "" || password == "" {
			return fmt.Errorf("%s requires MCP_AUTH_%s_USERNAME and MCP_AUTH_%s_PASSWORD", scheme.Name, envSchemeName(scheme.Name), envSchemeName(scheme.Name))
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
		applied.Headers.Set("Authorization", "Basic "+encoded)
		return nil

	case scheme.Type == "apiKey":
		key := r.envValue(scheme.Name, "KEY")
		if key == "" && strings.EqualFold(scheme.In, "header") && strings.EqualFold(scheme.ParameterName, "Authorization") {
			// Swagger 2.0 represents bearer tokens as apiKey in Authorization header.
			// Fall back to OIDC token resolution.
			token, err := r.resolveBearerToken(ctx, scheme.Name, true)
			if err != nil {
				return fmt.Errorf("%s requires MCP_AUTH_%s_KEY or an OIDC login", scheme.Name, envSchemeName(scheme.Name))
			}
			applied.Headers.Set("Authorization", "Bearer "+token)
			return nil
		}
		if key == "" {
			return fmt.Errorf("%s requires MCP_AUTH_%s_KEY", scheme.Name, envSchemeName(scheme.Name))
		}
		switch strings.ToLower(scheme.In) {
		case "header":
			name := scheme.ParameterName
			if name == "" {
				name = scheme.Name
			}
			applied.Headers.Set(name, key)
			return nil
		case "query":
			name := scheme.ParameterName
			if name == "" {
				name = scheme.Name
			}
			applied.Query.Set(name, key)
			return nil
		case "cookie":
			name := scheme.ParameterName
			if name == "" {
				name = scheme.Name
			}
			applied.Cookies[name] = key
			return nil
		default:
			return fmt.Errorf("%s uses unsupported apiKey location %q", scheme.Name, scheme.In)
		}

	case scheme.Type == "oauth2", scheme.Type == "openIdConnect":
		token, err := r.resolveBearerToken(ctx, scheme.Name, true)
		if err != nil {
			return err
		}
		applied.Headers.Set("Authorization", "Bearer "+token)
		return nil

	default:
		return fmt.Errorf("%s uses unsupported security scheme type=%q scheme=%q", scheme.Name, scheme.Type, scheme.Scheme)
	}
}

func (r *Resolver) resolveBearerToken(ctx context.Context, schemeName string, allowOIDC bool) (string, error) {
	if token := r.envValue(schemeName, "TOKEN"); token != "" {
		return token, nil
	}
	if r.globalBearer != "" {
		return r.globalBearer, nil
	}
	if !allowOIDC {
		return "", fmt.Errorf("%s requires a bearer token", schemeName)
	}

	provider, err := r.oidcProvider()
	if err != nil {
		return "", fmt.Errorf("%s requires MCP_AUTH_%s_TOKEN, MCP_AUTH_TOKEN, or an OIDC login for profile %q", schemeName, envSchemeName(schemeName), r.profile)
	}
	token, err := provider.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("%s could not load an OIDC token for profile %q: %w", schemeName, r.profile, err)
	}
	if token == "" {
		return "", fmt.Errorf("%s resolved to an empty bearer token", schemeName)
	}
	return token, nil
}

func (r *Resolver) oidcProvider() (TokenProvider, error) {
	if r.oidcLoaded {
		return r.oidc, r.oidcErr
	}
	r.oidcLoaded = true
	r.oidc, r.oidcErr = NewOIDCTokenProvider(TokenFilePath(r.profile))
	return r.oidc, r.oidcErr
}

func normalizeProfile(profile string) string {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return "default"
	}
	return profile
}

func envValue(schemeName, suffix string) string {
	return strings.TrimSpace(os.Getenv("MCP_AUTH_" + envSchemeName(schemeName) + "_" + suffix))
}

func envSchemeName(name string) string {
	name = strings.ToUpper(name)
	replacer := strings.NewReplacer(
		"-", "_",
		".", "_",
		"/", "_",
		" ", "_",
	)
	name = replacer.Replace(name)
	for strings.Contains(name, "__") {
		name = strings.ReplaceAll(name, "__", "_")
	}
	return strings.Trim(name, "_")
}

func dedupeStrings(items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}
