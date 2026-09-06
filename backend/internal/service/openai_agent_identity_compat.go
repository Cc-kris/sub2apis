package service

import (
	"net/http"
	"strings"
)

// CodexIdentityModeExtraKey is the account extra field used to select the
// Codex OAuth request identity policy.
const CodexIdentityModeExtraKey = "codex_identity_mode"

// OpenAICodexIdentityMode controls which account-owned Codex identity fields
// are added to an outbound request. Disabled is the safe default.
type OpenAICodexIdentityMode string

const (
	OpenAICodexIdentityModeDisabled OpenAICodexIdentityMode = "disabled"
	OpenAICodexIdentityModeDevice   OpenAICodexIdentityMode = "device"
	OpenAICodexIdentityModeSession  OpenAICodexIdentityMode = "session"
	OpenAICodexIdentityModeFull     OpenAICodexIdentityMode = "full"

	// Short aliases keep call sites that use the account-field terminology
	// readable without introducing a second enum.
	CodexIdentityModeDisabled = OpenAICodexIdentityModeDisabled
	CodexIdentityModeDevice   = OpenAICodexIdentityModeDevice
	CodexIdentityModeSession  = OpenAICodexIdentityModeSession
	CodexIdentityModeFull     = OpenAICodexIdentityModeFull
)

const (
	codexIdentityInstallationHeader = "x-codex-installation-id"
	codexIdentitySessionHeader      = "session_id"
)

// OpenAIAgentIdentityCompat is an account-scoped identity projection shared by
// HTTP, WebSocket and compact/probe request builders.
//
// Headers and Metadata contain only non-authentication identity values.  They
// never contain access/refresh/id tokens, API keys, or generated identifiers.
// Callers receive fresh maps from the resolver and may safely modify them.
type OpenAIAgentIdentityCompat struct {
	Mode     OpenAICodexIdentityMode
	Headers  http.Header
	Metadata map[string]any
}

// OpenAIAgentIdentity is a concise alias for the compatibility projection.
type OpenAIAgentIdentity = OpenAIAgentIdentityCompat

// GetCodexIdentityMode parses accounts.extra.codex_identity_mode.  Missing,
// malformed, non-OAuth, and unknown values all resolve to disabled so an
// account that predates this setting follows the existing request path.
func (a *Account) GetCodexIdentityMode() OpenAICodexIdentityMode {
	if a == nil || !a.IsOpenAIOAuth() || a.Extra == nil {
		return OpenAICodexIdentityModeDisabled
	}
	raw, ok := a.Extra[CodexIdentityModeExtraKey].(string)
	if !ok {
		return OpenAICodexIdentityModeDisabled
	}
	switch OpenAICodexIdentityMode(strings.ToLower(strings.TrimSpace(raw))) {
	case OpenAICodexIdentityModeDevice:
		return OpenAICodexIdentityModeDevice
	case OpenAICodexIdentityModeSession:
		return OpenAICodexIdentityModeSession
	case OpenAICodexIdentityModeFull:
		return OpenAICodexIdentityModeFull
	case OpenAICodexIdentityModeDisabled:
		return OpenAICodexIdentityModeDisabled
	default:
		return OpenAICodexIdentityModeDisabled
	}
}

// ResolveOpenAIAgentIdentityCompat returns the account-owned Codex identity
// fields for all outbound transports.  The mode is hierarchical:
// device adds installation identity, session adds account session identity,
// and full additionally pins the account User-Agent.  Empty account values
// are omitted; this resolver never invents replacements.
func ResolveOpenAIAgentIdentityCompat(account *Account) OpenAIAgentIdentityCompat {
	identity := OpenAIAgentIdentityCompat{
		Mode:     OpenAICodexIdentityModeDisabled,
		Headers:  make(http.Header),
		Metadata: make(map[string]any),
	}
	if account == nil {
		return identity
	}
	identity.Mode = account.GetCodexIdentityMode()
	if identity.Mode == OpenAICodexIdentityModeDisabled {
		return identity
	}

	if deviceID := strings.TrimSpace(account.GetOpenAIDeviceID()); deviceID != "" {
		identity.Headers.Set(codexIdentityInstallationHeader, deviceID)
		identity.Metadata[codexIdentityInstallationHeader] = deviceID
	}

	if identity.Mode == OpenAICodexIdentityModeSession || identity.Mode == OpenAICodexIdentityModeFull {
		if sessionID := strings.TrimSpace(account.GetOpenAISessionID()); sessionID != "" {
			identity.Headers.Set(codexIdentitySessionHeader, sessionID)
		}
	}

	if identity.Mode == OpenAICodexIdentityModeFull {
		if userAgent := strings.TrimSpace(account.GetOpenAIUserAgent()); userAgent != "" {
			identity.Headers.Set("User-Agent", userAgent)
		}
	}
	return identity
}

// ResolveOpenAIAgentIdentity is the short public resolver used by request
// builders that do not need the compatibility suffix in the function name.
func ResolveOpenAIAgentIdentity(account *Account) OpenAIAgentIdentityCompat {
	return ResolveOpenAIAgentIdentityCompat(account)
}

// resolveOpenAIAgentIdentity is kept package-local for HTTP/WS/probe helpers
// and makes the policy easy to consume without exposing mutable account data.
func resolveOpenAIAgentIdentity(account *Account) OpenAIAgentIdentityCompat {
	return ResolveOpenAIAgentIdentityCompat(account)
}

// ResolveOpenAIAgentIdentityHeaders returns a fresh request-header map.
func ResolveOpenAIAgentIdentityHeaders(account *Account) http.Header {
	return ResolveOpenAIAgentIdentityCompat(account).Headers
}

// ResolveOpenAIAgentIdentityMetadata returns a fresh client_metadata map.
func ResolveOpenAIAgentIdentityMetadata(account *Account) map[string]any {
	return ResolveOpenAIAgentIdentityCompat(account).Metadata
}

// ApplyTo merges this projection into an outbound request and client_metadata
// map.  Only policy-owned keys are written; unrelated caller metadata remains
// untouched.  Nil destinations are accepted as a no-op for that destination.
func (identity OpenAIAgentIdentityCompat) ApplyTo(headers http.Header, metadata map[string]any) {
	if headers != nil {
		for key, values := range identity.Headers {
			if len(values) == 0 {
				continue
			}
			headers.Set(key, values[0])
		}
	}
	if metadata != nil {
		for key, value := range identity.Metadata {
			metadata[key] = value
		}
	}
}
