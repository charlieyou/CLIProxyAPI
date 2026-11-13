package helps

import (
	"bytes"

	"github.com/tidwall/gjson"
)

// ClaudeStreamSummary records response shape without retaining text, thinking,
// tool arguments, signatures, or arbitrary upstream strings.
type ClaudeStreamSummary struct {
	stopReason     string
	textBytes      int
	contentBlocks  int
	toolUseBlocks  int
	thinkingBlocks int
	outputTokens   int64
	hasUsage       bool
}

// Observe reads a single SSE line, before response translation.
func (s *ClaudeStreamSummary) Observe(line []byte) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if !gjson.ValidBytes(payload) {
		return
	}
	root := gjson.ParseBytes(payload)
	var tokens gjson.Result
	switch root.Get("type").String() {
	case "message_start":
		tokens = root.Get("message.usage.output_tokens")
	case "content_block_start":
		s.contentBlocks++
		block := root.Get("content_block")
		switch block.Get("type").String() {
		case "text":
			s.textBytes += len(block.Get("text").String())
		case "tool_use":
			s.toolUseBlocks++
		case "thinking", "redacted_thinking":
			s.thinkingBlocks++
		}
	case "content_block_delta":
		delta := root.Get("delta")
		if delta.Get("type").String() == "text_delta" {
			s.textBytes += len(delta.Get("text").String())
		}
	case "message_delta":
		if reason := root.Get("delta.stop_reason"); reason.Type == gjson.String {
			switch reason.String() {
			case "end_turn", "max_tokens", "stop_sequence", "tool_use", "pause_turn", "refusal", "model_context_window_exceeded":
				s.stopReason = reason.String()
			default:
				s.stopReason = "unknown"
			}
		}
		tokens = root.Get("usage.output_tokens")
	}
	if tokens.Type == gjson.Number {
		s.outputTokens = tokens.Int()
		s.hasUsage = true
	}
}

// Fields returns only allowlisted metadata for the latency diagnostic log.
func (s *ClaudeStreamSummary) Fields() map[string]any {
	fields := map[string]any{
		"stop_reason":     s.stopReason,
		"text_bytes":      s.textBytes,
		"content_blocks":  s.contentBlocks,
		"tool_use_blocks": s.toolUseBlocks,
		"thinking_blocks": s.thinkingBlocks,
	}
	if s.hasUsage {
		fields["output_tokens"] = s.outputTokens
	}
	return fields
}

// IsClaudeTokenEvent reports whether an Anthropic Messages SSE chunk carries
// substantive output token content (such as text delta, thinking delta, or tool input delta).
// It filters out message_start, ping, content_block_stop, and empty containers.
func IsClaudeTokenEvent(payload []byte) bool {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return false
	}

	// Strip "event: ..." line if present in multi-line SSE buffer
	if bytes.HasPrefix(payload, []byte("event:")) {
		if idx := bytes.IndexByte(payload, '\n'); idx != -1 {
			payload = bytes.TrimSpace(payload[idx+1:])
		}
	}

	// Handle SSE data line prefix (e.g., "data: {...}")
	if bytes.HasPrefix(payload, []byte("data:")) {
		prefixLen := len("data:")
		if bytes.HasPrefix(payload, []byte("data: ")) {
			prefixLen = len("data: ")
		}
		payload = bytes.TrimSpace(payload[prefixLen:])
		if len(payload) == 0 {
			return false
		}
	}

	eventType := gjson.GetBytes(payload, "type").String()
	switch eventType {
	// Substantive streaming block deltas
	case "content_block_delta":
		delta := gjson.GetBytes(payload, "delta")
		if len(delta.Get("text").String()) > 0 {
			return true
		}
		if len(delta.Get("thinking").String()) > 0 {
			return true
		}
		if len(delta.Get("partial_json").String()) > 0 {
			return true
		}
		if len(delta.Get("signature").String()) > 0 {
			return true
		}
		return false

	// Initial block start when populated upfront
	case "content_block_start":
		cb := gjson.GetBytes(payload, "content_block")
		if len(cb.Get("text").String()) > 0 || len(cb.Get("thinking").String()) > 0 {
			return true
		}
		return false

	// Terminal completion events fallback
	case "message_delta":
		return len(gjson.GetBytes(payload, "delta.stop_reason").String()) > 0
	case "message_stop", "error":
		return true

	// Handshake metadata and container boundaries
	case "message_start", "ping", "content_block_stop":
		return false

	default:
		// Non-streaming / complete message payload fallback
		if gjson.GetBytes(payload, "content").IsArray() {
			for _, block := range gjson.GetBytes(payload, "content").Array() {
				if len(block.Get("text").String()) > 0 || len(block.Get("thinking").String()) > 0 {
					return true
				}
				if block.Get("type").String() == "tool_use" && len(block.Get("name").String()) > 0 {
					return true
				}
			}
		}
		return false
	}
}

// ObserveClaudeTokenEvent inspects an Anthropic Messages SSE chunk and records TTFT if the frame
// represents the first meaningful token event. It records first-packet arrival time as fallback
// and returns immediately with zero allocations once effective token TTFT is set.
func ObserveClaudeTokenEvent(reporter *UsageReporter, payload []byte) {
	if reporter != nil && len(payload) > 0 {
		reporter.ObserveResponseModel(payload)
	}
	if reporter == nil || len(payload) == 0 {
		return
	}
	if reporter.IsTTFTSet() {
		return
	}
	reporter.ObserveTokenEvent(IsClaudeTokenEvent(payload))
}
