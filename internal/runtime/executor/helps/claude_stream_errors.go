package helps

import (
	"bytes"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

const (
	claudeStreamPreContentMaxLines = 64
	claudeStreamPreContentMaxBytes = 32 * 1024
)

// ClaudeStreamPreContentGate holds the message_start, ping and error frames an
// Anthropic stream sends before its first content block. While nothing has
// reached the client, an upstream error event can still fail the whole attempt,
// so the auth manager is able to retry it on another credential.
type ClaudeStreamPreContentGate struct {
	committed         bool
	held              [][]byte
	heldBytes         int
	awaitingErrorData bool
}

// Committed reports whether the gate stopped holding lines.
func (g *ClaudeStreamPreContentGate) Committed() bool {
	return g.committed
}

// Hold buffers line while the stream has produced only pre-content frames and
// the buffer limits allow it. Once it returns false the gate is committed and
// the caller must forward Release() and then line.
func (g *ClaudeStreamPreContentGate) Hold(line []byte) bool {
	if g.committed {
		return false
	}
	if !isClaudeStreamPreContentLine(line) ||
		len(g.held) >= claudeStreamPreContentMaxLines ||
		g.heldBytes+len(line) > claudeStreamPreContentMaxBytes {
		g.committed = true
		return false
	}
	g.held = append(g.held, bytes.Clone(line))
	g.heldBytes += len(line)
	trimmed := bytes.TrimSpace(line)
	if event, ok := claudeSSEFieldValue(trimmed, "event"); ok {
		g.awaitingErrorData = strings.EqualFold(event, "error")
	} else if bytes.HasPrefix(trimmed, []byte("data:")) {
		g.awaitingErrorData = false
	}
	return true
}

// Release returns the held lines in arrival order and empties the gate.
func (g *ClaudeStreamPreContentGate) Release() [][]byte {
	held := g.held
	g.held = nil
	g.heldBytes = 0
	return held
}

// AwaitingErrorData reports whether the last held event header was
// "event: error" and no data line followed it.
func (g *ClaudeStreamPreContentGate) AwaitingErrorData() bool {
	return g.awaitingErrorData
}

func isClaudeStreamPreContentLine(line []byte) bool {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte(":")) ||
		bytes.HasPrefix(trimmed, []byte("id:")) || bytes.HasPrefix(trimmed, []byte("retry:")) {
		return true
	}
	if event, ok := claudeSSEFieldValue(trimmed, "event"); ok {
		switch strings.ToLower(event) {
		case "error", "message_start", "ping":
			return true
		default:
			return false
		}
	}
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return false
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 {
		return true
	}
	if !ojson.ValidBytes(payload) {
		return false
	}
	switch ojson.GetBytesNoCopy(payload, "type").String() {
	case "error", "message_start", "ping":
		return true
	default:
		return false
	}
}

func claudeSSEFieldValue(trimmed []byte, field string) (string, bool) {
	if !bytes.HasPrefix(trimmed, []byte(field)) || !bytes.HasPrefix(trimmed[len(field):], []byte(":")) {
		return "", false
	}
	return strings.TrimSpace(string(trimmed[len(field)+1:])), true
}

// ClaudeStreamErrorPayload returns the JSON payload of an SSE data line whose
// Anthropic event type is "error".
func ClaudeStreamErrorPayload(line []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil, false
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || !bytes.Contains(payload, []byte("error")) || !ojson.ValidBytes(payload) {
		return nil, false
	}
	if ojson.GetBytesNoCopy(payload, "type").String() != "error" {
		return nil, false
	}
	return payload, true
}

// ClaudeStreamErrorStatus classifies an Anthropic SSE error payload into the
// HTTP status the auth manager should see and a message that keeps the
// upstream error type.
func ClaudeStreamErrorStatus(payload []byte) (int, string) {
	root := util.ParseGJSONBytesNoCopy(payload)
	message := strings.TrimSpace(root.Get("error.message").String())
	errType := strings.TrimSpace(root.Get("error.type").String())
	if message == "" {
		message = errType
	} else if errType != "" && !strings.Contains(message, errType) {
		message = errType + ": " + message
	}
	if message == "" {
		message = "unknown upstream error"
	}

	lowerType := strings.ToLower(errType)
	lowerCode := strings.ToLower(strings.TrimSpace(root.Get("error.code").String()))
	lowerMessage := strings.ToLower(message)
	lowerRaw := strings.ToLower(root.Raw)
	switch {
	case strings.Contains(lowerType, "invalid_request") || strings.Contains(lowerCode, "invalid_request") ||
		strings.Contains(lowerRaw, "invalid_request_error"):
		return http.StatusBadRequest, message
	case strings.Contains(lowerType, "rate_limit") || strings.Contains(lowerType, "usage_limit") ||
		strings.Contains(lowerCode, "rate_limit") || strings.Contains(lowerCode, "usage_limit") ||
		strings.Contains(lowerMessage, "session limit") || strings.Contains(lowerMessage, "usage limit") ||
		strings.Contains(lowerMessage, "rate limit") || strings.Contains(lowerMessage, "quota") ||
		strings.Contains(lowerMessage, "too many requests"):
		return http.StatusTooManyRequests, message
	case strings.Contains(lowerType, "authentication") || strings.Contains(lowerCode, "auth") ||
		strings.Contains(lowerMessage, "invalid api key") || strings.Contains(lowerMessage, "expired token") ||
		strings.Contains(lowerMessage, "invalid or expired token"):
		return http.StatusUnauthorized, message
	case strings.Contains(lowerType, "permission") || strings.Contains(lowerCode, "permission") ||
		strings.Contains(lowerMessage, "permission") || strings.Contains(lowerMessage, "forbidden") ||
		strings.Contains(lowerMessage, "payment") || strings.Contains(lowerMessage, "billing") ||
		strings.Contains(lowerMessage, "credit"):
		return http.StatusForbidden, message
	case strings.Contains(lowerType, "overloaded") || strings.Contains(lowerCode, "overloaded"):
		return http.StatusServiceUnavailable, message
	default:
		return http.StatusBadGateway, message
	}
}
