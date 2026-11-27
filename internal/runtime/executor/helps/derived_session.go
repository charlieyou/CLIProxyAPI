package helps

import (
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/google/uuid"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

// DerivedSessionID returns the first context-derived session identity in metadata order.
func DerivedSessionID(metadataSets ...map[string]any) string {
	for _, metadata := range metadataSets {
		if derivedID := cliproxysession.DerivedID(metadata); derivedID != "" {
			return derivedID
		}
	}
	return ""
}

// DerivedSessionUUID maps a derived session identity to a provider-scoped stable UUID.
func DerivedSessionUUID(provider string, metadataSets ...map[string]any) string {
	return stableProviderSessionUUID(provider, "derived-session", DerivedSessionID(metadataSets...))
}

// ProviderSessionUUID prefers a long-lived execution session and falls back to the derived identity.
func ProviderSessionUUID(provider string, metadataSets ...map[string]any) string {
	for _, metadata := range metadataSets {
		if executionID := metadataString(metadata, cliproxyexecutor.ExecutionSessionMetadataKey); executionID != "" {
			return stableProviderSessionUUID(provider, "execution-session", executionID)
		}
	}
	return DerivedSessionUUID(provider, metadataSets...)
}

func stableProviderSessionUUID(provider string, kind string, identityValue string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	identityValue = strings.TrimSpace(identityValue)
	if provider == "" || identityValue == "" {
		return ""
	}
	identity := strings.Join([]string{"cli-proxy-api", provider, kind, identityValue}, "\x00")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(identity)).String()
}

// HashAntigravitySessionID hashes input with 64-bit FNV-1a and formats the
// signed int64 value as a base-10 decimal string, matching Antigravity Hub's
// request.sessionId format (~50% positive, ~50% negative).
func HashAntigravitySessionID(input []byte) string {
	h := fnv.New64a()
	_, _ = h.Write(input)
	return strconv.FormatInt(int64(h.Sum64()), 10)
}

// DerivedAntigravitySessionID maps a derived session identity to Antigravity's
// signed 64-bit FNV-1a decimal format (matching agy's ccpa.HashSessionID).
func DerivedAntigravitySessionID(metadataSets ...map[string]any) string {
	derivedID := DerivedSessionID(metadataSets...)
	if derivedID == "" {
		return ""
	}
	return AntigravityHashSessionID("cli-proxy-api:antigravity:derived-session\x00" + derivedID)
}

// ExplicitAntigravitySessionID returns an explicit client-supplied conversation
// identity (from CanonicalSessionIDMetadataKey or ExecutionSessionMetadataKey),
// ignoring prompt-derived (lcp:/ctx:/derived:), per-request (clientreq:), and
// per-user (user:) identities so unrelated conversations and turns do not collide
// or bust prompt caches.
func ExplicitAntigravitySessionID(metadataSets ...map[string]any) string {
	for _, key := range []string{
		cliproxyexecutor.CanonicalSessionIDMetadataKey,
		cliproxyexecutor.ExecutionSessionMetadataKey,
	} {
		for _, metadata := range metadataSets {
			if metadata == nil {
				continue
			}
			raw := metadataString(metadata, key)
			if raw == "" {
				continue
			}
			lower := strings.ToLower(raw)
			if strings.HasPrefix(lower, "lcp:") ||
				strings.HasPrefix(lower, "ctx:") ||
				strings.HasPrefix(lower, "derived:") ||
				strings.HasPrefix(lower, "clientreq:") ||
				strings.HasPrefix(lower, "user:") {
				continue
			}
			return AntigravityHashSessionID("cli-proxy-api:antigravity:explicit-session\x00" + raw)
		}
	}
	return ""
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}
