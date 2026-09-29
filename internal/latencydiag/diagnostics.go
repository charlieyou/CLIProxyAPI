// Package latencydiag records opt-in, metadata-only request latency timelines.
package latencydiag

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	log "github.com/sirupsen/logrus"
)

const (
	EnvDirectory  = "CLIPROXY_LATENCY_DIAGNOSTIC_DIR"
	maxFileBytes  = int64(8 << 20)
	maxTotalBytes = int64(64 << 20)
)

type contextKey struct{}

type recorder struct {
	dir string
	key []byte
	mu  sync.Mutex
}

type Timeline struct {
	recorder           *recorder
	id                 string
	started            time.Time
	hasher             hash.Hash
	mu                 sync.Mutex
	bytesIn            int64
	bytesOut           int64
	events             int64
	last               time.Time
	upstreamLast       time.Time
	upstreamMaxGap     time.Duration
	downstreamMaxWrite time.Duration
	done               chan struct{}
	endOnce            sync.Once
}

type record struct {
	Time                 time.Time      `json:"time"`
	RequestID            string         `json:"request_id"`
	Event                string         `json:"event"`
	ElapsedMS            int64          `json:"elapsed_ms"`
	BytesIn              int64          `json:"bytes_in,omitempty"`
	BytesOut             int64          `json:"bytes_out,omitempty"`
	EventCount           int64          `json:"event_count,omitempty"`
	IdleMS               int64          `json:"idle_ms,omitempty"`
	BodyHMAC             string         `json:"body_hmac,omitempty"`
	UpstreamIdleMS       int64          `json:"upstream_idle_ms,omitempty"`
	UpstreamMaxGapMS     int64          `json:"upstream_max_gap_ms,omitempty"`
	DownstreamMaxWriteMS int64          `json:"downstream_max_write_ms,omitempty"`
	Fields               map[string]any `json:"fields,omitempty"`
}

// MiddlewareFromEnv returns a no-op unless EnvDirectory names a writable directory.
func MiddlewareFromEnv() gin.HandlerFunc {
	dir := strings.TrimSpace(os.Getenv(EnvDirectory))
	if dir == "" {
		return func(c *gin.Context) { c.Next() }
	}
	r, err := newRecorder(dir)
	if err != nil {
		log.WithError(err).Error("latency diagnostics disabled")
		return func(c *gin.Context) { c.Next() }
	}
	return r.middleware
}

func newRecorder(dir string) (*recorder, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create diagnostic directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure diagnostic directory: %w", err)
	}
	keyPath := filepath.Join(dir, "diagnostic.key")
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err == nil {
			err = os.WriteFile(keyPath, key, 0o600)
		}
	}
	if err != nil || len(key) < 32 {
		return nil, fmt.Errorf("load diagnostic key: %w", err)
	}
	if err = os.Chmod(keyPath, 0o600); err != nil {
		return nil, fmt.Errorf("secure diagnostic key: %w", err)
	}
	return &recorder{dir: dir, key: key}, nil
}

func (r *recorder) middleware(c *gin.Context) {
	now := time.Now()
	t := &Timeline{recorder: r, id: uuid.NewString(), started: now, hasher: hmac.New(sha256.New, r.key), last: now, done: make(chan struct{})}
	if id := logging.GetRequestID(c.Request.Context()); id != "" {
		t.id = id
	}
	c.Header("X-Request-Id", t.id)
	ctx := context.WithValue(c.Request.Context(), contextKey{}, t)
	c.Request = c.Request.WithContext(ctx)
	if c.Request.Body != nil {
		c.Request.Body = &bodyReader{ReadCloser: c.Request.Body, timeline: t}
	}
	c.Writer = &responseWriter{ResponseWriter: c.Writer, timeline: t}
	t.emit("received", map[string]any{"method": c.Request.Method, "path": c.Request.URL.Path})
	go t.statusLoop(15 * time.Second)
	go func(requestDone <-chan struct{}) {
		select {
		case <-requestDone:
			t.emit("client_cancellation", nil)
		case <-t.done:
		}
	}(c.Request.Context().Done())
	c.Next()
	t.endOnce.Do(func() {
		close(t.done)
		t.emit("end", map[string]any{"status": c.Writer.Status()})
	})
}

type bodyReader struct {
	io.ReadCloser
	timeline *Timeline
	done     bool
}

func (r *bodyReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.timeline.mu.Lock()
		_, _ = r.timeline.hasher.Write(p[:n])
		r.timeline.bytesIn += int64(n)
		r.timeline.last = time.Now()
		r.timeline.mu.Unlock()
	}
	if err == io.EOF && !r.done {
		r.done = true
		r.timeline.mu.Lock()
		fingerprint := hex.EncodeToString(r.timeline.hasher.Sum(nil))[:24]
		r.timeline.mu.Unlock()
		r.timeline.emitRecord("body_finished", nil, fingerprint)
	}
	return n, err
}

type responseWriter struct {
	gin.ResponseWriter
	timeline  *Timeline
	writeOnce sync.Once
	flushOnce sync.Once
}

func (w *responseWriter) Write(p []byte) (int, error) {
	w.writeOnce.Do(func() { w.timeline.emit("first_downstream_write", nil) })
	start := time.Now()
	n, err := w.ResponseWriter.Write(p)
	w.timeline.mu.Lock()
	w.timeline.downstreamMaxWrite = max(w.timeline.downstreamMaxWrite, time.Since(start))
	w.timeline.bytesOut += int64(n)
	w.timeline.last = time.Now()
	w.timeline.mu.Unlock()
	return n, err
}

func (w *responseWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *responseWriter) Flush() {
	start := time.Now()
	w.ResponseWriter.Flush()
	w.timeline.mu.Lock()
	w.timeline.downstreamMaxWrite = max(w.timeline.downstreamMaxWrite, time.Since(start))
	w.timeline.mu.Unlock()
	w.flushOnce.Do(func() { w.timeline.emit("first_downstream_flush", nil) })
}

func (t *Timeline) statusLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.emit("active_status", nil)
		case <-t.done:
			return
		}
	}
}

func (t *Timeline) emit(event string, fields map[string]any) { t.emitRecord(event, fields, "") }

func (t *Timeline) emitRecord(event string, fields map[string]any, bodyHMAC string) {
	if t == nil || t.recorder == nil {
		return
	}
	now := time.Now()
	t.mu.Lock()
	t.events++
	rec := record{Time: now.UTC(), RequestID: t.id, Event: event, ElapsedMS: now.Sub(t.started).Milliseconds(), BytesIn: t.bytesIn, BytesOut: t.bytesOut, EventCount: t.events, IdleMS: now.Sub(t.last).Milliseconds(), BodyHMAC: bodyHMAC, Fields: fields}
	if !t.upstreamLast.IsZero() {
		rec.UpstreamIdleMS = now.Sub(t.upstreamLast).Milliseconds()
	}
	rec.UpstreamMaxGapMS = t.upstreamMaxGap.Milliseconds()
	rec.DownstreamMaxWriteMS = t.downstreamMaxWrite.Milliseconds()
	if event != "active_status" {
		t.last = now
	}
	t.mu.Unlock()
	if err := t.recorder.write(rec); err != nil {
		log.WithError(err).Warn("latency diagnostic record dropped")
	}
}

func (r *recorder) write(rec record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	path := filepath.Join(r.dir, "latency.jsonl")
	if info, statErr := os.Stat(path); statErr == nil && info.Size()+int64(len(b)+1) > maxFileBytes {
		rotated := filepath.Join(r.dir, fmt.Sprintf("latency-%d.jsonl", time.Now().UnixNano()))
		if err = os.Rename(path, rotated); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(b, '\n')); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	r.pruneLocked()
	return err
}

func (r *recorder) pruneLocked() {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	type fileInfo struct {
		path string
		size int64
		mod  time.Time
	}
	var files []fileInfo
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo == nil {
			files = append(files, fileInfo{filepath.Join(r.dir, entry.Name()), info.Size(), info.ModTime()})
			total += info.Size()
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, file := range files {
		if total <= maxTotalBytes || filepath.Base(file.path) == "latency.jsonl" {
			continue
		}
		if os.Remove(file.path) == nil {
			total -= file.size
		}
	}
}

// Event appends a safe metadata event to the request timeline.
func Event(ctx context.Context, name string, fields map[string]any) {
	if t := FromContext(ctx); t != nil {
		t.emit(name, fields)
	}
}

// Propagate copies diagnostic metadata without altering cancellation ownership.
func Propagate(parent, request context.Context) context.Context {
	if t := FromContext(request); t != nil {
		return context.WithValue(parent, contextKey{}, t)
	}
	return parent
}

// FromContext returns the active diagnostic timeline.
func FromContext(ctx context.Context) *Timeline {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(contextKey{}).(*Timeline)
	return t
}

// ObserveUpstream updates stream activity counters without logging chunks.
func ObserveUpstream(ctx context.Context) {
	if t := FromContext(ctx); t != nil {
		now := time.Now()
		t.mu.Lock()
		if !t.upstreamLast.IsZero() {
			t.upstreamMaxGap = max(t.upstreamMaxGap, now.Sub(t.upstreamLast))
		}
		t.upstreamLast = now
		t.mu.Unlock()
	}
}

// Opaque fingerprints a value with the dedicated diagnostic key.
func Opaque(ctx context.Context, value string) string {
	t := FromContext(ctx)
	if t == nil || value == "" {
		return ""
	}
	h := hmac.New(sha256.New, t.recorder.key)
	_, _ = io.WriteString(h, value)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// SafeUpstreamRequestID extracts only conventional opaque request ID headers.
func SafeUpstreamRequestID(header http.Header) string {
	for _, key := range []string{"request-id", "x-request-id", "anthropic-request-id"} {
		if value := strings.TrimSpace(header.Get(key)); value != "" && len(value) <= 256 {
			return value
		}
	}
	return ""
}
