package signature

import (
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// GeminiSkipThoughtSignatureValidatorBase64 is the protojson base64 encoding of
// []byte("skip_thought_signature_validator") emitted on the Antigravity wire
// for Part.thought_signature (protobuf TYPE_BYTES).
const GeminiSkipThoughtSignatureValidatorBase64 = "c2tpcF90aG91Z2h0X3NpZ25hdHVyZV92YWxpZGF0b3I="

// IsAntigravityThoughtSignatureBypass reports whether rawSignature is one of
// Gemini's documented bypass sentinels, including the protojson base64 form
// that the agy Antigravity wire carries. Other providers keep using
// IsGeminiThoughtSignatureBypass, which does not accept the base64 form.
func IsAntigravityThoughtSignatureBypass(rawSignature string) bool {
	return IsGeminiThoughtSignatureBypass(rawSignature) || strings.TrimSpace(rawSignature) == GeminiSkipThoughtSignatureValidatorBase64
}

// StripExtraContentGoogleThoughtSignatureAtPath removes every occurrence of
// extra_content.google.thought_signature from the part at partPath and prunes
// empty google and extra_content wrapper objects while preserving non-empty siblings.
func StripExtraContentGoogleThoughtSignatureAtPath(payload []byte, partPath string) ([]byte, bool) {
	if len(payload) == 0 || partPath == "" {
		return payload, false
	}
	rebuilt, changed := stripExtraContentGoogleThoughtSignaturePartRaw(util.GetGJSONBytesNoCopy(payload, partPath))
	if !changed {
		return payload, false
	}
	updated, errSet := ojson.SetRawBytes(payload, partPath, []byte(rebuilt))
	if errSet != nil {
		return payload, false
	}
	return updated, true
}

// StripExtraContentGoogleThoughtSignaturePartBytes applies the same stripping and
// empty-wrapper pruning to a standalone part JSON payload.
func StripExtraContentGoogleThoughtSignaturePartBytes(part []byte) ([]byte, bool) {
	if len(part) == 0 {
		return part, false
	}
	rebuilt, changed := stripExtraContentGoogleThoughtSignaturePartRaw(util.ParseGJSONBytesNoCopy(part))
	if !changed {
		return part, false
	}
	return []byte(rebuilt), true
}
