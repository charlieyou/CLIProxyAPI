package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestAntigravityToolDescriptionReplacementsReachUpstreamBody(t *testing.T) {
	captured := make(chan []byte, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read request body: %v", errRead)
			return
		}
		captured <- body
		switch {
		case strings.Contains(r.URL.Path, "countTokens"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"totalTokens":42}`))
		case strings.Contains(r.URL.Path, "streamGenerateContent"):
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}}\n\n"))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1,"totalTokenCount":11}}}`))
		}
	}))
	defer server.Close()

	executor := NewAntigravityExecutor(&config.Config{
		RequestRetry: 1,
		Antigravity: config.AntigravityConfig{
			ToolDescriptionReplacements: map[string]string{
				"an Amp thread":      "a session thread",
				"An Amp thread":      "A session thread",
				"Amp attachment URL": "attachment URL",
			},
		},
	})
	auth := &cliproxyauth.Auth{
		ID:         "antigravity-tool-desc-auth",
		Provider:   "antigravity",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	payload := []byte(`{
		"systemInstruction": {"parts": [{"text": "Keep an Amp thread in systemInstruction."}]},
		"contents": [{"role": "user", "parts": [{"text": "Keep an Amp thread in contents."}]}],
		"tools": [{
			"functionDeclarations": [
				{
					"name": "manage_amp",
					"description": "Inspect <target> on an Amp thread.",
					"parametersJsonSchema": {
						"type": "object",
						"$defs": {"Target": {"type": "string", "description": "An Amp thread ref target"}},
						"properties": {
							"attachment": {"type": "string", "description": "An Amp attachment URL"},
							"mode": {"type": "string", "enum": ["an Amp thread", "other"]},
							"target": {"$ref": "#/$defs/Target"}
						},
						"required": ["attachment"]
					}
				},
				{
					"name": "bare_mcp_tool",
					"description": "Create task",
					"parameters": {
						"thread_id": {"type": "string", "description": "An Amp thread ID"}
					}
				}
			]
		}]
	}`)
	req := cliproxyexecutor.Request{Model: "gemini-3.6-flash-high", Payload: payload}
	opts := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatGemini,
		ResponseFormat:  sdktranslator.FormatGemini,
		OriginalRequest: payload,
	}
	streamOpts := opts
	streamOpts.Stream = true

	if _, errExec := executor.Execute(context.Background(), auth, req, opts); errExec != nil {
		t.Fatalf("Execute() error = %v", errExec)
	}
	assertAntigravityToolDescriptionsScrubbed(t, "execute", <-captured)

	result, errStream := executor.ExecuteStream(context.Background(), auth, req, streamOpts)
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}
	assertAntigravityToolDescriptionsScrubbed(t, "stream", <-captured)

	if _, errCount := executor.CountTokens(context.Background(), auth, req, opts); errCount != nil {
		t.Fatalf("CountTokens() error = %v", errCount)
	}
	assertAntigravityToolDescriptionsScrubbed(t, "count-tokens", <-captured)

	// Claude models take the executeClaudeNonStream path and the Claude tool
	// schema cleaner.
	claudeReq := cliproxyexecutor.Request{Model: "claude-sonnet-4-6", Payload: payload}
	if _, errExec := executor.Execute(context.Background(), auth, claudeReq, opts); errExec != nil {
		t.Fatalf("Execute(claude) error = %v", errExec)
	}
	assertAntigravityToolDescriptionsScrubbed(t, "claude-execute", <-captured)
}

func assertAntigravityToolDescriptionsScrubbed(t *testing.T, label string, body []byte) {
	t.Helper()
	decls := ojson.GetBytes(body, "request.tools.0.functionDeclarations")
	schemaOf := func(decl ojson.Result) ojson.Result {
		if schema := decl.Get("parametersJsonSchema"); schema.Exists() {
			return schema
		}
		return decl.Get("parameters")
	}
	decl := decls.Get("0")
	if got := decl.Get("name").String(); got != "manage_amp" {
		t.Fatalf("%s: tool name = %q, want manage_amp; body=%s", label, got, body)
	}
	if got := decl.Get("description").String(); got != "Inspect <target> on a session thread." {
		t.Fatalf("%s: tool description = %q; body=%s", label, got, body)
	}
	schema := schemaOf(decl)
	if got := schema.Get("properties.attachment.description").String(); got != "An attachment URL" {
		t.Fatalf("%s: param description = %q; body=%s", label, got, body)
	}
	// Schema cleaning may fold enums into description hints; enum values that
	// survive must stay verbatim.
	if enum := schema.Get("properties.mode.enum"); enum.Exists() && enum.Get("0").String() != "an Amp thread" {
		t.Fatalf("%s: enum mutated: %s; body=%s", label, enum.Raw, body)
	}
	if got := schema.Get("properties.target.description").String(); got != "A session thread ref target" {
		t.Fatalf("%s: $ref target description = %q; body=%s", label, got, body)
	}
	if got := schemaOf(decls.Get("1")).Get("properties.thread_id.description").String(); got != "A session thread ID" {
		t.Fatalf("%s: bare MCP property map description = %q; body=%s", label, got, body)
	}
	if got := ojson.GetBytes(body, "request.systemInstruction.parts.0.text").String(); got != "Keep an Amp thread in systemInstruction." {
		t.Fatalf("%s: systemInstruction mutated: %q", label, got)
	}
	if got := ojson.GetBytes(body, "request.contents.0.parts.0.text").String(); got != "Keep an Amp thread in contents." {
		t.Fatalf("%s: contents mutated: %q", label, got)
	}
}
