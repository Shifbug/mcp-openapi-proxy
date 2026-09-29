package auth

import (
	"os"
	"strings"
)

// CredentialEnvRefs maps one OpenAPI security scheme to app-scoped
// environment-variable names. Values are looked up only at request time, so
// secret material never needs to be copied into the MCP configuration object.
type CredentialEnvRefs struct {
	KeyEnv      string
	TokenEnv    string
	UsernameEnv string
	PasswordEnv string
}

// NewResolverWithCredentialEnv creates a resolver whose configured schemes use
// app-scoped environment references instead of the process-global
// MCP_AUTH_<SCHEME>_* convention. Schemes not present in refs keep the legacy
// resolver behavior for backwards compatibility.
func NewResolverWithCredentialEnv(profile string, refs map[string]CredentialEnvRefs) *Resolver {
	r := NewResolver(profile)
	if len(refs) == 0 {
		return r
	}
	r.credentialEnv = make(map[string]CredentialEnvRefs, len(refs))
	for scheme, ref := range refs {
		r.credentialEnv[scheme] = ref
	}
	return r
}

func (r *Resolver) envValue(schemeName, suffix string) string {
	if r != nil && r.credentialEnv != nil {
		if ref, ok := r.credentialEnv[schemeName]; ok {
			var envName string
			switch suffix {
			case "KEY":
				envName = ref.KeyEnv
			case "TOKEN":
				envName = ref.TokenEnv
			case "USERNAME":
				envName = ref.UsernameEnv
			case "PASSWORD":
				envName = ref.PasswordEnv
			}
			if envName == "" {
				return ""
			}
			return strings.TrimSpace(os.Getenv(envName))
		}
	}
	return envValue(schemeName, suffix)
}
