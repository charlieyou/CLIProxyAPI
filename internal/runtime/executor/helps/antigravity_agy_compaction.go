package helps

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
)

const (
	agyAntigravityCompactionCapsulePrefix = "cpa-ag-compact-v1:"
	agyFixedCompactionKeySecret           = "CLIProxyAPI"
)

type agyAntigravityCompactionCapsuleData struct {
	Summary   string `json:"summary"`
	Model     string `json:"model"`
	CreatedAt int64  `json:"created_at"`
}

// HasResponsesCompactionTrigger checks whether input contains a compaction_trigger item.
func AgyHasResponsesCompactionTrigger(payload []byte) bool {
	input := ojson.GetBytes(payload, "input")
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		if item.Get("type").String() == "compaction_trigger" {
			return true
		}
	}
	return false
}

// HasResponsesCompactionItem checks whether input contains a compaction item.
func AgyHasResponsesCompactionItem(payload []byte) bool {
	input := ojson.GetBytes(payload, "input")
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		if item.Get("type").String() == "compaction" {
			return true
		}
	}
	return false
}

// PrepareAntigravityCompactionSummaryPayload prepares a payload for non-stream Antigravity summary generation.
func AgyPrepareAntigravityCompactionSummaryPayload(payload []byte, modelName string) []byte {
	out := payload
	summaryPrompt := `{"type":"message","role":"user","content":[{"type":"input_text","text":"Please provide a concise and comprehensive summary of the preceding conversation and task progress so far, including user goals, key findings, actions taken, and current status, so that work can continue smoothly."}]}`

	input := ojson.GetBytes(out, "input")
	if input.IsArray() {
		var filtered []string
		for _, item := range input.Array() {
			itemType := item.Get("type").String()
			if itemType == "compaction_trigger" {
				continue
			}
			filtered = append(filtered, item.Raw)
		}
		filtered = append(filtered, summaryPrompt)
		newInput := "[" + strings.Join(filtered, ",") + "]"
		out, _ = ojson.SetRawBytes(out, "input", []byte(newInput))
	} else if input.Type == ojson.String && input.String() != "" {
		userMsg := []byte(`{"type":"message","role":"user","content":[{"type":"input_text"}]}`)
		userMsg, _ = ojson.SetBytes(userMsg, "content.0.text", input.String())
		newInput := "[" + string(userMsg) + "," + summaryPrompt + "]"
		out, _ = ojson.SetRawBytes(out, "input", []byte(newInput))
	} else {
		newInput := "[" + summaryPrompt + "]"
		out, _ = ojson.SetRawBytes(out, "input", []byte(newInput))
	}

	// Remove fields not needed or incompatible with summary generation
	for _, field := range []string{
		"stream",
		"tools",
		"tool_choice",
		"previous_response_id",
		"parallel_tool_calls",
		"additional_tools",
		"truncation",
		"metadata",
	} {
		out, _ = ojson.DeleteBytes(out, field)
	}

	out, _ = ojson.SetBytes(out, "stream", false)
	return out
}

// deriveAntigravityCompactionKey derives an AES-256 key from the fixed CLIProxyAPI secret.
func agyDeriveAntigravityCompactionKey() []byte {
	h := sha256.Sum256([]byte(agyFixedCompactionKeySecret))
	return h[:]
}

// SealAntigravityCompaction encrypts summary data into an opaque capsule using AES-GCM.
func AgySealAntigravityCompaction(summary, modelName string) (string, error) {
	data := agyAntigravityCompactionCapsuleData{
		Summary:   summary,
		Model:     modelName,
		CreatedAt: time.Now().Unix(),
	}
	plaintext, errMarshal := json.Marshal(data)
	if errMarshal != nil {
		return "", fmt.Errorf("marshal compaction capsule: %w", errMarshal)
	}

	key := agyDeriveAntigravityCompactionKey()
	block, errCipher := aes.NewCipher(key)
	if errCipher != nil {
		return "", fmt.Errorf("create cipher: %w", errCipher)
	}

	gcm, errGCM := cipher.NewGCM(block)
	if errGCM != nil {
		return "", fmt.Errorf("create gcm: %w", errGCM)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, errRead := io.ReadFull(rand.Reader, nonce); errRead != nil {
		return "", fmt.Errorf("generate nonce: %w", errRead)
	}

	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return agyAntigravityCompactionCapsulePrefix + base64.RawURLEncoding.EncodeToString(ciphertext), nil
}

// UnsealAntigravityCompaction decrypts and validates an opaque capsule.
func AgyUnsealAntigravityCompaction(encryptedContent string) (string, error) {
	if !strings.HasPrefix(encryptedContent, agyAntigravityCompactionCapsulePrefix) {
		return "", fmt.Errorf("unrecognized compaction capsule format")
	}

	raw := strings.TrimPrefix(encryptedContent, agyAntigravityCompactionCapsulePrefix)
	ciphertext, errDecode := base64.RawURLEncoding.DecodeString(raw)
	if errDecode != nil {
		return "", fmt.Errorf("decode compaction capsule: %w", errDecode)
	}

	key := agyDeriveAntigravityCompactionKey()
	block, errCipher := aes.NewCipher(key)
	if errCipher != nil {
		return "", fmt.Errorf("create cipher: %w", errCipher)
	}

	gcm, errGCM := cipher.NewGCM(block)
	if errGCM != nil {
		return "", fmt.Errorf("create gcm: %w", errGCM)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize+gcm.Overhead() {
		return "", fmt.Errorf("compaction capsule ciphertext too short")
	}

	nonce, encrypted := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, errOpen := gcm.Open(nil, nonce, encrypted, nil)
	if errOpen != nil {
		return "", fmt.Errorf("invalid or corrupted compaction capsule: %w", errOpen)
	}

	var data agyAntigravityCompactionCapsuleData
	if errUnmarshal := json.Unmarshal(plaintext, &data); errUnmarshal != nil {
		return "", fmt.Errorf("unmarshal compaction capsule: %w", errUnmarshal)
	}
	return data.Summary, nil
}

// ExpandAntigravityCompactionCapsules expands compaction items in input into developer context.
func AgyExpandAntigravityCompactionCapsules(payload []byte) ([]byte, error) {
	input := ojson.GetBytes(payload, "input")
	if !input.IsArray() {
		return payload, nil
	}

	var expandedItems []string
	changed := false
	for _, item := range input.Array() {
		if item.Get("type").String() == "compaction" {
			encryptedContent := item.Get("encrypted_content").String()
			summary, errUnseal := AgyUnsealAntigravityCompaction(encryptedContent)
			if errUnseal != nil {
				return nil, fmt.Errorf("invalid compaction capsule: %w", errUnseal)
			}
			devMsg := []byte(`{"type":"message","role":"developer","content":[{"type":"input_text"}]}`)
			devMsg, _ = ojson.SetBytes(devMsg, "content.0.text", "Context summary from previous turns:\n"+summary)
			expandedItems = append(expandedItems, string(devMsg))
			changed = true
			continue
		}
		expandedItems = append(expandedItems, item.Raw)
	}

	if !changed {
		return payload, nil
	}

	newInput := "[" + strings.Join(expandedItems, ",") + "]"
	return ojson.SetRawBytes(payload, "input", []byte(newInput))
}

// ExtractAntigravitySummaryText extracts summary text from Antigravity or Responses response.
func AgyExtractAntigravitySummaryText(respPayload []byte) (string, error) {
	// 1. OpenAI Responses format: iterate through output array and skip reasoning
	output := ojson.GetBytes(respPayload, "output")
	if output.IsArray() {
		var textParts []string
		for _, item := range output.Array() {
			if item.Get("type").String() == "message" {
				content := item.Get("content")
				if content.IsArray() {
					for _, part := range content.Array() {
						if part.Get("type").String() == "output_text" {
							if t := part.Get("text").String(); t != "" {
								textParts = append(textParts, t)
							}
						}
					}
				} else if content.Type == ojson.String && content.String() != "" {
					textParts = append(textParts, content.String())
				}
			}
		}
		if len(textParts) > 0 {
			return strings.Join(textParts, "\n"), nil
		}
	}

	// 2. Antigravity Gemini format: response.candidates.0.content.parts or candidates.0.content.parts
	candidates := ojson.GetBytes(respPayload, "response.candidates")
	if !candidates.Exists() {
		candidates = ojson.GetBytes(respPayload, "candidates")
	}
	if candidates.IsArray() && len(candidates.Array()) > 0 {
		var textParts []string
		parts := candidates.Array()[0].Get("content.parts")
		if parts.IsArray() {
			for _, part := range parts.Array() {
				if part.Get("thought").Bool() {
					continue
				}
				if t := part.Get("text").String(); t != "" {
					textParts = append(textParts, t)
				}
			}
		}
		if len(textParts) > 0 {
			return strings.Join(textParts, "\n"), nil
		}
	}

	// 3. Claude format: content array
	content := ojson.GetBytes(respPayload, "content")
	if content.IsArray() {
		var textParts []string
		for _, block := range content.Array() {
			if block.Get("type").String() == "text" {
				if t := block.Get("text").String(); t != "" {
					textParts = append(textParts, t)
				}
			}
		}
		if len(textParts) > 0 {
			return strings.Join(textParts, "\n"), nil
		}
	}

	// 4. OpenAI Chat format: choices.0.message.content
	if text := ojson.GetBytes(respPayload, "choices.0.message.content").String(); text != "" {
		return text, nil
	}

	return "", fmt.Errorf("no summary text found in upstream response")
}

// BuildAntigravityCompactionStreamChunks creates SSE frames for compaction stream response.
func AgyBuildAntigravityCompactionStreamChunks(modelName, capsule string, inputTokens, outputTokens, totalTokens int) [][]byte {
	now := time.Now().Unix()
	responseID := fmt.Sprintf("resp_ag_compact_%d", time.Now().UnixNano())
	itemID := fmt.Sprintf("cmp_ag_compact_%d", time.Now().UnixNano())

	itemInProgress := []byte(`{"type":"compaction","status":"in_progress"}`)
	itemInProgress, _ = ojson.SetBytes(itemInProgress, "id", itemID)
	itemInProgress, _ = ojson.SetBytes(itemInProgress, "encrypted_content", capsule)

	itemCompleted := []byte(`{"type":"compaction","status":"completed"}`)
	itemCompleted, _ = ojson.SetBytes(itemCompleted, "id", itemID)
	itemCompleted, _ = ojson.SetBytes(itemCompleted, "encrypted_content", capsule)

	usageNode := []byte(`{"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}`)
	usageNode, _ = ojson.SetBytes(usageNode, "input_tokens", inputTokens)
	usageNode, _ = ojson.SetBytes(usageNode, "output_tokens", outputTokens)
	usageNode, _ = ojson.SetBytes(usageNode, "total_tokens", totalTokens)

	createdResponse := []byte(`{"object":"response","status":"in_progress","background":false,"error":null,"output":[]}`)
	createdResponse, _ = ojson.SetBytes(createdResponse, "id", responseID)
	createdResponse, _ = ojson.SetBytes(createdResponse, "created_at", now)
	createdResponse, _ = ojson.SetBytes(createdResponse, "model", modelName)

	inProgressResponse := createdResponse

	completedResponse := []byte(`{"object":"response","status":"completed","background":false,"error":null}`)
	completedResponse, _ = ojson.SetBytes(completedResponse, "id", responseID)
	completedResponse, _ = ojson.SetBytes(completedResponse, "created_at", now)
	completedResponse, _ = ojson.SetBytes(completedResponse, "completed_at", now)
	completedResponse, _ = ojson.SetBytes(completedResponse, "model", modelName)
	completedResponse, _ = ojson.SetRawBytes(completedResponse, "output", []byte("["+string(itemCompleted)+"]"))
	completedResponse, _ = ojson.SetRawBytes(completedResponse, "usage", usageNode)

	createdPayload := []byte(`{"type":"response.created","sequence_number":0}`)
	createdPayload, _ = ojson.SetRawBytes(createdPayload, "response", createdResponse)

	inProgressPayload := []byte(`{"type":"response.in_progress","sequence_number":1}`)
	inProgressPayload, _ = ojson.SetRawBytes(inProgressPayload, "response", inProgressResponse)

	addedPayload := []byte(`{"type":"response.output_item.added","sequence_number":2,"output_index":0}`)
	addedPayload, _ = ojson.SetRawBytes(addedPayload, "item", itemInProgress)

	donePayload := []byte(`{"type":"response.output_item.done","sequence_number":3,"output_index":0}`)
	donePayload, _ = ojson.SetRawBytes(donePayload, "item", itemCompleted)

	completedPayload := []byte(`{"type":"response.completed","sequence_number":4}`)
	completedPayload, _ = ojson.SetRawBytes(completedPayload, "response", completedResponse)

	frames := [][]byte{
		agyBuildSSEFrame("response.created", createdPayload),
		agyBuildSSEFrame("response.in_progress", inProgressPayload),
		agyBuildSSEFrame("response.output_item.added", addedPayload),
		agyBuildSSEFrame("response.output_item.done", donePayload),
		agyBuildSSEFrame("response.completed", completedPayload),
	}
	return frames
}

// BuildAntigravityCompactionResponse creates JSON response for non-stream response.compaction.
func AgyBuildAntigravityCompactionResponse(modelName, capsule string, inputTokens, outputTokens, totalTokens int) []byte {
	now := time.Now().Unix()
	responseID := fmt.Sprintf("resp_ag_compact_%d", time.Now().UnixNano())
	itemID := fmt.Sprintf("cmp_ag_compact_%d", time.Now().UnixNano())

	itemJSON := []byte(`{"type":"compaction","status":"completed"}`)
	itemJSON, _ = ojson.SetBytes(itemJSON, "id", itemID)
	itemJSON, _ = ojson.SetBytes(itemJSON, "encrypted_content", capsule)

	usageJSON := []byte(`{"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}`)
	usageJSON, _ = ojson.SetBytes(usageJSON, "input_tokens", inputTokens)
	usageJSON, _ = ojson.SetBytes(usageJSON, "output_tokens", outputTokens)
	usageJSON, _ = ojson.SetBytes(usageJSON, "total_tokens", totalTokens)

	respJSON := []byte(`{"object":"response.compaction","status":"completed"}`)
	respJSON, _ = ojson.SetBytes(respJSON, "id", responseID)
	respJSON, _ = ojson.SetBytes(respJSON, "created_at", now)
	respJSON, _ = ojson.SetBytes(respJSON, "model", modelName)
	respJSON, _ = ojson.SetRawBytes(respJSON, "output", []byte("["+string(itemJSON)+"]"))
	respJSON, _ = ojson.SetRawBytes(respJSON, "usage", usageJSON)

	return respJSON
}

func agyBuildSSEFrame(event string, data []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("event: ")
	buf.WriteString(event)
	buf.WriteString("\ndata: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	return buf.Bytes()
}
