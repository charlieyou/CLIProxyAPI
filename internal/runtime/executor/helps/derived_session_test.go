package helps

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/google/uuid"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func assertSignedInt64SessionID(t *testing.T, label, value string) int64 {
	t.Helper()
	parsed, errParse := strconv.ParseInt(value, 10, 64)
	if errParse != nil || strconv.FormatInt(parsed, 10) != value {
		t.Fatalf("%s = %q, want canonical signed int64 decimal (err=%v)", label, value, errParse)
	}
	return parsed
}

func TestDerivedSessionProviderMappings(t *testing.T) {
	t.Parallel()

	metadata := map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:test-root"}
	codexID := DerivedSessionUUID("codex", metadata)
	xaiID := DerivedSessionUUID("xai", metadata)
	if _, errParse := uuid.Parse(codexID); errParse != nil {
		t.Fatalf("Codex mapping %q is not a UUID: %v", codexID, errParse)
	}
	if _, errParse := uuid.Parse(xaiID); errParse != nil {
		t.Fatalf("xAI mapping %q is not a UUID: %v", xaiID, errParse)
	}
	if codexID == xaiID {
		t.Fatalf("provider namespaces produced the same UUID: %q", codexID)
	}
	if repeated := DerivedSessionUUID("codex", metadata); repeated != codexID {
		t.Fatalf("Codex mapping is not stable: first=%q repeated=%q", codexID, repeated)
	}

	antigravityID := DerivedAntigravitySessionID(metadata)
	assertSignedInt64SessionID(t, "Antigravity mapping", antigravityID)
	if repeated := DerivedAntigravitySessionID(metadata); repeated != antigravityID {
		t.Fatalf("Antigravity mapping is not stable: first=%q repeated=%q", antigravityID, repeated)
	}
}

func TestProviderSessionUUIDPrefersExecutionSession(t *testing.T) {
	t.Parallel()

	first := map[string]any{
		cliproxyexecutor.ExecutionSessionMetadataKey: "connection-1",
		cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:first-root",
	}
	second := map[string]any{
		cliproxyexecutor.ExecutionSessionMetadataKey: "connection-1",
		cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:second-root",
	}
	firstID := ProviderSessionUUID("codex", first)
	secondID := ProviderSessionUUID("codex", second)
	if firstID == "" || firstID != secondID {
		t.Fatalf("execution session did not stabilize provider UUID: first=%q second=%q", firstID, secondID)
	}
	if firstID == DerivedSessionUUID("codex", first) {
		t.Fatalf("provider UUID did not prefer execution session: %q", firstID)
	}
}

func TestDerivedSessionProviderMappingsRequireIdentity(t *testing.T) {
	t.Parallel()

	if got := DerivedSessionUUID("codex", nil); got != "" {
		t.Fatalf("DerivedSessionUUID() = %q, want empty", got)
	}
	if got := DerivedAntigravitySessionID(nil); got != "" {
		t.Fatalf("DerivedAntigravitySessionID() = %q, want empty", got)
	}
}

func TestHashAntigravitySessionIDMatchesFNV1a64AndIncludesBothSigns(t *testing.T) {
	t.Parallel()

	referenceFNV1a64 := func(input []byte) string {
		var h uint64 = 0xcbf29ce484222325
		for _, b := range input {
			h = (h ^ uint64(b)) * 0x100000001b3
		}
		return strconv.FormatInt(int64(h), 10)
	}

	samples := []string{"", "a", "hello", "session-1", "session-2", "cli-proxy-api\x00antigravity\x00canonical-session\x00auth-a\x00sess-1"}
	for _, sample := range samples {
		got := HashAntigravitySessionID([]byte(sample))
		want := referenceFNV1a64([]byte(sample))
		if got != want {
			t.Fatalf("HashAntigravitySessionID(%q) = %q, want %q", sample, got, want)
		}
		assertSignedInt64SessionID(t, fmt.Sprintf("HashAntigravitySessionID(%q)", sample), got)
	}

	for i := 0; i < 64; i++ {
		assertSignedInt64SessionID(t, "DerivedAntigravitySessionID", DerivedAntigravitySessionID(map[string]any{
			cliproxyexecutor.DerivedSessionIDMetadataKey: fmt.Sprintf("ctx:v1:session-%d", i),
		}))
	}
}
