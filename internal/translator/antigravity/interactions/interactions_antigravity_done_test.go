package interactions

import (
	"bytes"
	"context"
	"testing"
)

// main (e29156a5e) parsed a bare `[DONE]` chunk with gjson, which read it as
// an array holding the scalar `NE`. It never ran its `[DONE]` branch, so the
// chunk emits nothing once the stream has started and only opens the
// interaction when it has not.
func TestConvertAntigravityResponseToInteractionsBareDoneMatchesMain(t *testing.T) {
	var param any
	ConvertAntigravityResponseToInteractions(context.Background(), "m", nil, nil, []byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}]}}`), &param)
	if events := ConvertAntigravityResponseToInteractions(context.Background(), "m", nil, nil, []byte("[DONE]"), &param); len(events) != 0 {
		t.Fatalf("bare [DONE] after content emitted %q, want nothing", events)
	}

	var fresh any
	events := ConvertAntigravityResponseToInteractions(context.Background(), "m", nil, nil, []byte(" [DONE]\n"), &fresh)
	if len(events) != 2 ||
		!bytes.HasPrefix(events[0], []byte("event: interaction.created\n")) ||
		!bytes.HasPrefix(events[1], []byte("event: interaction.status_update\n")) {
		t.Fatalf("bare [DONE] on a fresh stream emitted %q, want interaction.created and interaction.status_update", events)
	}
	if events := ConvertAntigravityResponseToInteractions(context.Background(), "m", nil, nil, []byte("[DONE]"), &fresh); len(events) != 0 {
		t.Fatalf("second bare [DONE] emitted %q, want nothing", events)
	}

	var prefixed any
	events = ConvertAntigravityResponseToInteractions(context.Background(), "m", nil, nil, []byte("data: [DONE]"), &prefixed)
	if len(events) == 0 || !bytes.HasPrefix(events[len(events)-1], []byte("event: done\n")) {
		t.Fatalf("data: [DONE] emitted %q, want a final done event", events)
	}
}
