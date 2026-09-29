package latencydiag

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMiddlewareRecordsMetadataLifecycleWithoutBody(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDirectory, dir)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(MiddlewareFromEnv())
	engine.POST("/stream", func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		Event(c.Request.Context(), "test_upstream", map[string]any{"status": 200})
		_, _ = c.Writer.WriteString("ok")
		c.Writer.Flush()
	})

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/stream", strings.NewReader("super-secret-body"))
	engine.ServeHTTP(recorder, req)

	records := readRecords(t, filepath.Join(dir, "latency.jsonl"))
	want := map[string]bool{"received": false, "body_finished": false, "test_upstream": false, "first_downstream_write": false, "first_downstream_flush": false, "end": false}
	for _, rec := range records {
		want[rec.Event] = true
	}
	for event, found := range want {
		if !found {
			t.Errorf("event %q missing from %#v", event, records)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "latency.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "super-secret-body") {
		t.Fatal("diagnostic log leaked request body")
	}
	keyInfo, err := os.Stat(filepath.Join(dir, "diagnostic.key"))
	if err != nil || keyInfo.Mode().Perm() != 0o600 {
		t.Fatalf("diagnostic key mode = %v, err = %v", keyInfo.Mode().Perm(), err)
	}
	logInfo, err := os.Stat(filepath.Join(dir, "latency.jsonl"))
	if err != nil || logInfo.Mode().Perm() != 0o600 {
		t.Fatalf("diagnostic log mode = %v, err = %v", logInfo.Mode().Perm(), err)
	}
}

func TestMiddlewareDisabledCreatesNoFiles(t *testing.T) {
	t.Setenv(EnvDirectory, "")
	engine := gin.New()
	engine.Use(MiddlewareFromEnv())
	var timelinePresent bool
	engine.GET("/", func(c *gin.Context) {
		timelinePresent = FromContext(c.Request.Context()) != nil
		c.Status(http.StatusNoContent)
	})
	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if timelinePresent {
		t.Fatal("disabled middleware installed a diagnostic timeline")
	}
}

func TestRecorderRotatesAndPrunesOldFiles(t *testing.T) {
	dir := t.TempDir()
	r, err := newRecorder(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "latency.jsonl"), make([]byte, maxFileBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 9; i++ {
		path := filepath.Join(dir, "old-"+string(rune('a'+i))+".jsonl")
		if err = os.WriteFile(path, make([]byte, 8<<20), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err = r.write(record{Event: "rotation", RequestID: "test"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".jsonl") {
			info, _ := entry.Info()
			total += info.Size()
		}
	}
	if total > maxTotalBytes {
		t.Fatalf("JSONL total = %d, want <= %d", total, maxTotalBytes)
	}
}

func readRecords(t *testing.T, path string) []record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var records []record
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var rec record
		if err = json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		records = append(records, rec)
	}
	if err = scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}
