package helps

import (
	"bytes"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// FilterAgySSEUsageMetadata is v7's FilterSSEUsageMetadata as the agy
// Antigravity replication shipped it. v8's version renames the usageMetadata
// key in place, which leaves a duplicate cpaUsageMetadata key when the frame
// already carries one; v7 overwrote the existing cpaUsageMetadata value and
// deleted usageMetadata, so the upstream cpaUsageMetadata never reaches the
// client.
func FilterAgySSEUsageMetadata(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}

	lines := bytes.Split(payload, []byte("\n"))
	modified := false
	foundData := false
	for idx, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		foundData = true
		dataIdx := bytes.Index(line, []byte("data:"))
		if dataIdx < 0 {
			continue
		}
		rawJSON := bytes.TrimSpace(line[dataIdx+5:])
		traceID := strings.Clone(ojson.GetBytesNoCopy(rawJSON, "traceId").String())
		if agyIsStopChunkWithoutUsage(rawJSON) && traceID != "" {
			rememberStopWithoutUsage(traceID)
			continue
		}
		if traceID != "" {
			if _, ok := stopChunkWithoutUsage.Load(traceID); ok && hasUsageMetadata(rawJSON) {
				stopChunkWithoutUsage.Delete(traceID)
				continue
			}
		}

		cleaned, changed := stripAgyUsageMetadataFromJSON(rawJSON)
		if !changed {
			continue
		}
		var rebuilt []byte
		rebuilt = append(rebuilt, line[:dataIdx]...)
		rebuilt = append(rebuilt, "data:"...)
		if len(cleaned) > 0 {
			rebuilt = append(rebuilt, ' ')
			rebuilt = append(rebuilt, cleaned...)
		}
		lines[idx] = rebuilt
		modified = true
	}
	if !modified {
		if !foundData {
			// Handle payloads that are raw JSON without SSE data: prefix.
			cleaned, changed := stripAgyUsageMetadataFromJSON(bytes.TrimSpace(payload))
			if !changed {
				return payload
			}
			return cleaned
		}
		return payload
	}
	return bytes.Join(lines, []byte("\n"))
}

// stripAgyUsageMetadataFromJSON is v7's StripUsageMetadataFromJSON: unless the
// chunk is terminal, usageMetadata (top level or under response) moves to
// cpaUsageMetadata, replacing any cpaUsageMetadata already there.
func stripAgyUsageMetadataFromJSON(rawJSON []byte) ([]byte, bool) {
	jsonBytes := bytes.TrimSpace(rawJSON)
	if len(jsonBytes) == 0 || !ojson.ValidBytes(jsonBytes) {
		return rawJSON, false
	}

	finishReason := ojson.GetBytesNoCopy(jsonBytes, "candidates.0.finishReason")
	if !finishReason.Exists() {
		finishReason = ojson.GetBytesNoCopy(jsonBytes, "response.candidates.0.finishReason")
	}
	if finishReason.Exists() && strings.TrimSpace(finishReason.String()) != "" {
		return rawJSON, false
	}
	if !ojson.GetBytesNoCopy(jsonBytes, "usageMetadata").Exists() && !ojson.GetBytesNoCopy(jsonBytes, "response.usageMetadata").Exists() {
		return rawJSON, false
	}

	doc := ojson.NewDoc(jsonBytes)
	if usageMetadata := doc.Get("usageMetadata"); usageMetadata.Exists() {
		_ = doc.SetRaw("cpaUsageMetadata", strings.Clone(usageMetadata.Raw))
		_ = doc.Delete("usageMetadata")
	}
	if usageMetadata := doc.Get("response.usageMetadata"); usageMetadata.Exists() {
		_ = doc.SetRaw("response.cpaUsageMetadata", strings.Clone(usageMetadata.Raw))
		_ = doc.Delete("response.usageMetadata")
	}
	return doc.Bytes(), true
}

// agyIsStopChunkWithoutUsage is v7's isStopChunkWithoutUsage, without v8's
// raw-byte marker shortcuts.
func agyIsStopChunkWithoutUsage(jsonBytes []byte) bool {
	if len(jsonBytes) == 0 || !ojson.ValidBytes(jsonBytes) {
		return false
	}
	finishReason := ojson.GetBytesNoCopy(jsonBytes, "candidates.0.finishReason")
	if !finishReason.Exists() {
		finishReason = ojson.GetBytesNoCopy(jsonBytes, "response.candidates.0.finishReason")
	}
	if !finishReason.Exists() || strings.TrimSpace(finishReason.String()) == "" {
		return false
	}
	return !hasUsageMetadata(jsonBytes)
}
