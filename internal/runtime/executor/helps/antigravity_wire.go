package helps

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	internalsignature "github.com/router-for-me/CLIProxyAPI/v8/internal/signature"
	translatorcommon "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/sync/singleflight"
)

const (
	antigravityAnonymousScope            = "anonymous"
	antigravityConversationCacheCapacity = 4096
	antigravityConversationTTL           = 24 * time.Hour
	antigravityAccountStateCapacity      = 4096
	antigravityAICodeDefaultCooldown     = 15 * time.Minute
)

// AntigravityCredentialScope returns a stable, non-secret per-credential scope
// key shared across connection pooling, conversation tracking, and companion state.
// The credential digest is included alongside any stable ID/path/source so replacing
// the account behind an existing auth ID cannot reuse keep-alive TCP/TLS connections
// or companion state from the previous OAuth grant.
func AntigravityCredentialScope(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return antigravityAnonymousScope
	}
	credDigest := antigravityCredentialMaterialDigest(auth)
	identity := ""
	if id := strings.TrimSpace(auth.ID); id != "" {
		identity = "id:" + id
	} else if auth.Attributes != nil {
		if path := strings.TrimSpace(auth.Attributes[cliproxyauth.AttributePath]); path != "" {
			identity = "path:" + path
		} else if source := strings.TrimSpace(auth.Attributes[cliproxyauth.AttributeSource]); source != "" {
			identity = "source:" + source
		}
	}
	if identity != "" {
		if credDigest != "" {
			return identity + "|" + credDigest
		}
		if auth.RegistrationEpoch > 0 {
			return fmt.Sprintf("%s|epoch:%d", identity, auth.RegistrationEpoch)
		}
		return identity
	}
	if credDigest != "" {
		return credDigest
	}
	return antigravityAnonymousScope
}

func antigravityCredentialMaterialDigest(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	// Prefer refresh_token over access_token so ordinary access-token rotation
	// keeps the same pool scope while replacing the OAuth grant isolates it.
	if refresh, ok := auth.Metadata["refresh_token"].(string); ok && strings.TrimSpace(refresh) != "" {
		sum := sha256.Sum256([]byte(strings.TrimSpace(refresh)))
		return "refresh:" + hex.EncodeToString(sum[:8])
	}
	if access, ok := auth.Metadata["access_token"].(string); ok && strings.TrimSpace(access) != "" {
		sum := sha256.Sum256([]byte(strings.TrimSpace(access)))
		return "token:" + hex.EncodeToString(sum[:8])
	}
	return ""
}

// AntigravityConversation holds active UUIDv4 conversation identifiers and the
// frozen per-turn local timestamps for one multi-turn Antigravity trajectory.
type AntigravityConversation struct {
	CascadeID          string      `json:"cascade_id"`
	TrajectoryID       string      `json:"trajectory_id"`
	SessionID          string      `json:"session_id"`
	RootFingerprint    string      `json:"root_fingerprint"`
	CreatedAt          time.Time   `json:"created_at"`
	UserTurnTimestamps []time.Time `json:"user_turn_timestamps,omitempty"`
	UpdatedAt          int64       `json:"updated_at"`
	ACLWritten         bool        `json:"acl_written,omitempty"`
	Turn1Failed        bool        `json:"-"`
}

func cloneAntigravityConversation(conv AntigravityConversation) AntigravityConversation {
	if len(conv.UserTurnTimestamps) > 0 {
		conv.UserTurnTimestamps = append([]time.Time(nil), conv.UserTurnTimestamps...)
	}
	return conv
}

type antigravityConvNode struct {
	key  string
	conv AntigravityConversation
	elem *list.Element
}

type persistedAntigravitySessions struct {
	Entries map[string]AntigravityConversation `json:"entries"`
}

const (
	// Use a non-.json extension so FileTokenStore and Watcher (which match *.json
	// inside AuthDir) never treat the session cache as an auth credential file.
	antigravitySessionStateFilename = ".antigravity_sessions.state"
	antigravitySessionSaveDebounce  = 2 * time.Second
)

var (
	antigravityConvMu        sync.Mutex
	antigravityConvByKey     = make(map[string]*antigravityConvNode)
	antigravityConvByTraj    = make(map[string]*antigravityConvNode)
	antigravityConvLRU       = list.New()
	antigravityConvNow       = time.Now
	antigravityConvStorePath string
	antigravityConvSaveTimer *time.Timer
	antigravityConvDirty     bool
	antigravityConvPersistMu sync.Mutex
	antigravityLastActivity  atomic.Int64
)

// TouchAntigravityActivity records recent Antigravity inference traffic so
// background refresh loops stay quiet while the proxy is idle.
func TouchAntigravityActivity() {
	antigravityLastActivity.Store(time.Now().UnixNano())
}

// AntigravityLastActivity returns the timestamp of the most recent Antigravity
// inference request (zero if no inference has run since startup).
func AntigravityLastActivity() time.Time {
	ns := antigravityLastActivity.Load()
	if ns <= 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// ResetAntigravityWireStateForTest clears in-memory conversation and account
// state (for unit tests) and stops any pending debounced persistence timer.
func ResetAntigravityWireStateForTest() {
	antigravityConvMu.Lock()
	if antigravityConvSaveTimer != nil {
		antigravityConvSaveTimer.Stop()
		antigravityConvSaveTimer = nil
	}
	antigravityConvDirty = false
	antigravityConvStorePath = ""
	antigravityConvByKey = make(map[string]*antigravityConvNode)
	antigravityConvByTraj = make(map[string]*antigravityConvNode)
	antigravityConvLRU = list.New()
	antigravityConvNow = time.Now
	antigravityConvMu.Unlock()

	antigravityLastActivity.Store(0)

	antigravityAcctMu.Lock()
	antigravityAcctState = make(map[string]*AntigravityAccountState)
	antigravityAcctMu.Unlock()
}

// ConfigureAntigravitySessionStore enables bounded on-disk persistence of
// active Antigravity conversations in dir so Fly restarts/deploys preserve
// trajectory_id, cascade_id, sessionId, frozen Turn-1 timestamps, and ACL state.
// Passing an empty dir disables persistence.
func ConfigureAntigravitySessionStore(dir string) {
	dir = strings.TrimSpace(dir)
	var targetPath string
	if dir != "" {
		targetPath = filepath.Join(dir, antigravitySessionStateFilename)
	}

	antigravityConvMu.Lock()
	if antigravityConvSaveTimer != nil {
		antigravityConvSaveTimer.Stop()
		antigravityConvSaveTimer = nil
	}
	antigravityConvDirty = false
	antigravityConvStorePath = targetPath
	antigravityConvMu.Unlock()

	if targetPath == "" {
		return
	}
	loadAntigravitySessionStore(targetPath)
}

// FlushAntigravitySessionStore synchronously persists any pending conversation
// updates to disk (safe to call during server shutdown).
func FlushAntigravitySessionStore() {
	antigravityConvMu.Lock()
	if antigravityConvSaveTimer != nil {
		antigravityConvSaveTimer.Stop()
		antigravityConvSaveTimer = nil
	}
	path := antigravityConvStorePath
	dirty := antigravityConvDirty
	antigravityConvMu.Unlock()

	if path == "" || !dirty {
		return
	}
	persistAntigravitySessionSnapshot(path)
}

func loadAntigravitySessionStore(path string) {
	raw, errRead := os.ReadFile(path)
	if errRead != nil || len(raw) == 0 {
		return
	}
	var snap persistedAntigravitySessions
	if errUnmarshal := json.Unmarshal(raw, &snap); errUnmarshal != nil || len(snap.Entries) == 0 {
		return
	}
	type loadedItem struct {
		key  string
		conv AntigravityConversation
	}
	antigravityConvMu.Lock()
	defer antigravityConvMu.Unlock()

	cutoff := antigravityConvNow().Add(-antigravityConversationTTL).Unix()
	items := make([]loadedItem, 0, len(snap.Entries))
	for k, c := range snap.Entries {
		if k == "" || c.TrajectoryID == "" || c.CascadeID == "" || c.UpdatedAt < cutoff {
			continue
		}
		// Self-heal any legacy persisted entry whose session_id predated the
		// FNV64a(cascade_id) invariant enforced by agy.
		c.SessionID = AntigravityHashSessionID(c.CascadeID)
		c.Turn1Failed = false
		items = append(items, loadedItem{key: k, conv: c})
	}
	// Sort ascending by UpdatedAt so LRU head always holds the oldest entry
	// and TTL eviction is never head-of-line blocked.
	sort.Slice(items, func(i, j int) bool {
		if items[i].conv.UpdatedAt != items[j].conv.UpdatedAt {
			return items[i].conv.UpdatedAt < items[j].conv.UpdatedAt
		}
		return items[i].key < items[j].key
	})
	for _, item := range items {
		if existing := antigravityConvByKey[item.key]; existing != nil {
			if existing.elem != nil {
				antigravityConvLRU.Remove(existing.elem)
			}
			if antigravityConvByTraj[existing.conv.TrajectoryID] == existing {
				delete(antigravityConvByTraj, existing.conv.TrajectoryID)
			}
		}
		node := &antigravityConvNode{key: item.key, conv: item.conv}
		node.elem = antigravityConvLRU.PushBack(node)
		antigravityConvByKey[item.key] = node
		antigravityConvByTraj[item.conv.TrajectoryID] = node
	}
	evictAntigravityConvLocked()
}

func scheduleAntigravityConvPersistLocked() {
	if antigravityConvStorePath == "" {
		return
	}
	antigravityConvDirty = true
	if antigravityConvSaveTimer != nil {
		return
	}
	path := antigravityConvStorePath
	antigravityConvSaveTimer = time.AfterFunc(antigravitySessionSaveDebounce, func() {
		antigravityConvMu.Lock()
		antigravityConvSaveTimer = nil
		currentPath := antigravityConvStorePath
		dirty := antigravityConvDirty
		antigravityConvMu.Unlock()
		if currentPath != "" && currentPath == path && dirty {
			persistAntigravitySessionSnapshot(currentPath)
		}
	})
}

func persistAntigravitySessionSnapshot(path string) {
	antigravityConvPersistMu.Lock()
	defer antigravityConvPersistMu.Unlock()

	antigravityConvMu.Lock()
	if antigravityConvStorePath != path {
		antigravityConvMu.Unlock()
		return
	}
	evictAntigravityConvLocked()
	entries := make(map[string]AntigravityConversation, len(antigravityConvByKey))
	for k, node := range antigravityConvByKey {
		if node == nil {
			continue
		}
		c := cloneAntigravityConversation(node.conv)
		c.Turn1Failed = false
		entries[k] = c
	}
	antigravityConvDirty = false
	antigravityConvMu.Unlock()

	encoded, errMarshal := json.Marshal(persistedAntigravitySessions{Entries: entries})
	if errMarshal != nil {
		return
	}
	dir := filepath.Dir(path)
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return
	}
	tmp, errTemp := os.CreateTemp(dir, ".antigravity_sessions-*.tmp")
	if errTemp != nil {
		return
	}
	tmpPath := tmp.Name()
	if errChmod := tmp.Chmod(0o600); errChmod != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return
	}
	if _, errWrite := tmp.Write(encoded); errWrite != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return
	}
	if errClose := tmp.Close(); errClose != nil {
		_ = os.Remove(tmpPath)
		return
	}
	if errRename := os.Rename(tmpPath, path); errRename != nil {
		_ = os.Remove(tmpPath)
	}
}

// SetAntigravityWireNowFuncForTest overrides the conversation cache clock for
// deterministic unit tests. Pass nil to restore time.Now.
func SetAntigravityWireNowFuncForTest(fn func() time.Time) {
	antigravityConvMu.Lock()
	if fn == nil {
		antigravityConvNow = time.Now
	} else {
		antigravityConvNow = fn
	}
	antigravityConvMu.Unlock()
}

func evictAntigravityConvLocked() {
	cutoff := antigravityConvNow().Add(-antigravityConversationTTL).Unix()
	for antigravityConvLRU.Len() > 0 {
		front := antigravityConvLRU.Front()
		node, ok := front.Value.(*antigravityConvNode)
		if !ok || node == nil {
			antigravityConvLRU.Remove(front)
			continue
		}
		if antigravityConvLRU.Len() <= antigravityConversationCacheCapacity && node.conv.UpdatedAt >= cutoff {
			break
		}
		antigravityConvLRU.Remove(front)
		if antigravityConvByKey[node.key] == node {
			delete(antigravityConvByKey, node.key)
		}
		if antigravityConvByTraj[node.conv.TrajectoryID] == node {
			delete(antigravityConvByTraj, node.conv.TrajectoryID)
		}
	}
}

// HasAntigravityTrajectoryACLWritten reports whether writeTrajectoryAcls has
// already succeeded for trajectoryID in the current or persisted session cache.
func HasAntigravityTrajectoryACLWritten(trajectoryID string) bool {
	trajectoryID = strings.TrimSpace(trajectoryID)
	if trajectoryID == "" {
		return false
	}
	antigravityConvMu.Lock()
	defer antigravityConvMu.Unlock()
	if node := antigravityConvByTraj[trajectoryID]; node != nil {
		return node.conv.ACLWritten
	}
	return false
}

// MarkAntigravityTrajectoryACLWritten records that writeTrajectoryAcls succeeded
// for trajectoryID and persists the state when session storage is configured.
func MarkAntigravityTrajectoryACLWritten(trajectoryID string) {
	trajectoryID = strings.TrimSpace(trajectoryID)
	if trajectoryID == "" {
		return
	}
	antigravityConvMu.Lock()
	defer antigravityConvMu.Unlock()
	if node := antigravityConvByTraj[trajectoryID]; node != nil && !node.conv.ACLWritten {
		node.conv.ACLWritten = true
		scheduleAntigravityConvPersistLocked()
	}
}

// MarkAntigravityTurn1RetryableFailure flags trajectoryID so an immediate Turn-1
// retry on the same opening prompt reuses the existing cascade_id, trajectory_id,
// sessionId, and frozen Turn-1 timestamp instead of minting a duplicate trajectory.
func MarkAntigravityTurn1RetryableFailure(trajectoryID string) {
	trajectoryID = strings.TrimSpace(trajectoryID)
	if trajectoryID == "" {
		return
	}
	antigravityConvMu.Lock()
	defer antigravityConvMu.Unlock()
	if node := antigravityConvByTraj[trajectoryID]; node != nil {
		node.conv.Turn1Failed = true
	}
}

// ResolveAntigravityConversation resolves random UUIDv4 cascade_id and
// trajectory_id plus a signed 64-bit FNV-1a sessionId for a request.
//
// Turn 1 requests (llmTurnCount == 0) without an explicit client sessionId
// always allocate a fresh UUIDv4 trajectory (and a fresh sessionId whenever
// rootKey was already used by an earlier conversation) so separate threads that
// happen to share the same opening message never collide. Turn 2+ requests
// reuse the active trajectory's UUIDv4s and sessionId for prompt-cache hits
// and ACL deduplication.
func ResolveAntigravityConversation(
	credScope string,
	explicitSessionID string,
	derivedSessionID string,
	rootFingerprint string,
	llmTurnCount int,
) AntigravityConversation {
	credScope = strings.TrimSpace(credScope)
	if credScope == "" {
		credScope = "default"
	}
	explicitSessionID = strings.TrimSpace(explicitSessionID)
	derivedSessionID = strings.TrimSpace(derivedSessionID)
	rootFingerprint = strings.TrimSpace(rootFingerprint)

	hasExplicit := explicitSessionID != ""
	normalizedExplicit := ""
	if hasExplicit {
		if _, err := strconv.ParseInt(explicitSessionID, 10, 64); err == nil {
			normalizedExplicit = explicitSessionID
		} else {
			normalizedExplicit = AntigravityHashSessionID(explicitSessionID)
		}
	}

	var rootKey string
	switch {
	case hasExplicit:
		rootKey = "explicit:" + normalizedExplicit
	case derivedSessionID != "":
		rootKey = "derived:" + derivedSessionID
	case rootFingerprint != "":
		rootKey = "root:" + AntigravityHashSessionID(rootFingerprint)
	default:
		rootKey = "ephemeral:" + uuid.NewString()
	}
	storeKey := credScope + "|" + rootKey

	antigravityConvMu.Lock()
	defer antigravityConvMu.Unlock()

	now := antigravityConvNow()
	nowUnix := now.Unix()
	cutoff := now.Add(-antigravityConversationTTL).Unix()

	existing, hasExisting := antigravityConvByKey[storeKey]
	if hasExisting && (existing == nil || existing.conv.UpdatedAt < cutoff) {
		if existing != nil {
			if existing.elem != nil {
				antigravityConvLRU.Remove(existing.elem)
			}
			if antigravityConvByTraj[existing.conv.TrajectoryID] == existing {
				delete(antigravityConvByTraj, existing.conv.TrajectoryID)
			}
		}
		delete(antigravityConvByKey, storeKey)
		existing = nil
		hasExisting = false
	}

	isFirstTurn := llmTurnCount == 0
	// Reuse an existing conversation on Turn 2+, or on a Turn-1 retry when
	// the opening prompt matches (either under an explicit session or after a
	// retryable Turn-1 failure, avoiding duplicate trajectories on 429/503 retries).
	if hasExisting && (!isFirstTurn || (existing.conv.RootFingerprint == rootFingerprint && (hasExplicit || existing.conv.Turn1Failed))) {
		existing.conv.Turn1Failed = false
		existing.conv.SessionID = AntigravityHashSessionID(existing.conv.CascadeID)
		existing.conv.UpdatedAt = nowUnix
		antigravityConvLRU.MoveToBack(existing.elem)
		scheduleAntigravityConvPersistLocked()
		return cloneAntigravityConversation(existing.conv)
	}

	cascadeID := uuid.NewString()
	trajectoryID := uuid.NewString()
	// Match agy's ccpa.BuildRequest invariant unconditionally: request.sessionId
	// is always the signed 64-bit FNV-1a hash of request.labels.cascade_id.
	// explicitSessionID and derivedSessionID serve only as local conversation
	// lookup keys in antigravityConvByKey.
	sessionID := AntigravityHashSessionID(cascadeID)

	if existing != nil {
		if existing.elem != nil {
			antigravityConvLRU.Remove(existing.elem)
		}
		if antigravityConvByTraj[existing.conv.TrajectoryID] == existing {
			delete(antigravityConvByTraj, existing.conv.TrajectoryID)
		}
	}
	localNow := now
	node := &antigravityConvNode{
		key: storeKey,
		conv: AntigravityConversation{
			CascadeID:          cascadeID,
			TrajectoryID:       trajectoryID,
			SessionID:          sessionID,
			RootFingerprint:    rootFingerprint,
			CreatedAt:          localNow,
			UserTurnTimestamps: []time.Time{localNow},
			UpdatedAt:          nowUnix,
		},
	}
	node.elem = antigravityConvLRU.PushBack(node)
	antigravityConvByKey[storeKey] = node
	antigravityConvByTraj[trajectoryID] = node
	evictAntigravityConvLocked()
	scheduleAntigravityConvPersistLocked()
	return cloneAntigravityConversation(node.conv)
}

func antigravityNow() time.Time {
	antigravityConvMu.Lock()
	fn := antigravityConvNow
	antigravityConvMu.Unlock()
	if fn == nil {
		return time.Now()
	}
	return fn()
}

// ResolveAntigravityUserTurnTimestamp returns the frozen local timestamp for
// the given 0-based human user turn within trajectoryID, allocating and
// persisting one when a new follow-up user turn arrives so subsequent turns
// stay byte-identical for prompt caching.
func ResolveAntigravityUserTurnTimestamp(trajectoryID string, turnOrdinal int, fallback time.Time) time.Time {
	if turnOrdinal < 0 {
		turnOrdinal = 0
	}
	trajectoryID = strings.TrimSpace(trajectoryID)
	if trajectoryID == "" {
		if !fallback.IsZero() {
			return fallback
		}
		return antigravityNow()
	}

	antigravityConvMu.Lock()
	defer antigravityConvMu.Unlock()

	node := antigravityConvByTraj[trajectoryID]
	if node == nil {
		if !fallback.IsZero() {
			return fallback
		}
		return antigravityConvNow()
	}
	if turnOrdinal == 0 {
		if len(node.conv.UserTurnTimestamps) > 0 && !node.conv.UserTurnTimestamps[0].IsZero() {
			return node.conv.UserTurnTimestamps[0]
		}
		if !node.conv.CreatedAt.IsZero() {
			return node.conv.CreatedAt
		}
	}
	if turnOrdinal < len(node.conv.UserTurnTimestamps) && !node.conv.UserTurnTimestamps[turnOrdinal].IsZero() {
		return node.conv.UserTurnTimestamps[turnOrdinal]
	}

	ts := antigravityConvNow()
	if turnOrdinal == 0 && !fallback.IsZero() {
		ts = fallback
	}
	for len(node.conv.UserTurnTimestamps) <= turnOrdinal {
		node.conv.UserTurnTimestamps = append(node.conv.UserTurnTimestamps, time.Time{})
	}
	if node.conv.UserTurnTimestamps[0].IsZero() && !node.conv.CreatedAt.IsZero() {
		node.conv.UserTurnTimestamps[0] = node.conv.CreatedAt
	}
	node.conv.UserTurnTimestamps[turnOrdinal] = ts
	scheduleAntigravityConvPersistLocked()
	return ts
}

// NormalizeAntigravityFunctionResponseRoles sets role "model" on every
// request.contents turn whose parts are all functionResponse parts, matching
// agy 1.5.16, which tags tool results with role "model" for every model family.
// Turns that mix functionResponse with other parts keep their role, because
// relabeling user text as model output would change its meaning.
func NormalizeAntigravityFunctionResponseRoles(payload []byte) []byte {
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		return payload
	}
	var items [][]byte
	changed := false
	contents.ForEach(func(_, content ojson.Result) bool {
		raw := []byte(content.Raw)
		if content.Get("role").String() != "model" && antigravityContentIsFunctionResponseOnly(content) {
			if updated, errSet := ojson.SetBytes(raw, "role", "model"); errSet == nil {
				raw = updated
				changed = true
			}
		}
		items = append(items, raw)
		return true
	})
	if !changed {
		return payload
	}
	joined := append([]byte{'['}, bytes.Join(items, []byte{','})...)
	joined = append(joined, ']')
	if updated, errSet := ojson.SetRawBytes(payload, "request.contents", joined); errSet == nil {
		return updated
	}
	return payload
}

func antigravityContentIsFunctionResponseOnly(content ojson.Result) bool {
	parts := content.Get("parts")
	if !parts.IsArray() {
		return false
	}
	count := 0
	onlyResponses := true
	parts.ForEach(func(_, part ojson.Result) bool {
		count++
		if !part.Get("functionResponse").Exists() && !part.Get("function_response").Exists() {
			onlyResponses = false
			return false
		}
		return true
	})
	return count > 0 && onlyResponses
}

// NormalizeAntigravityFunctionResponseOutputs rewrites single-key string
// functionResponse.response objects from {"result":"..."} to {"output":"..."},
// matching agy's ccpa.buildStringFunctionResponsePart wire format across all
// request formats (including direct Gemini /v1beta payloads). Structured,
// non-string, or multi-key response objects are left untouched.
func NormalizeAntigravityFunctionResponseOutputs(payload []byte) []byte {
	return translatorcommon.NormalizeAntigravityFunctionResponseOutputs(payload)
}

// CountAntigravityAgentSteps computes agy's exact step indices from
// request.contents:
//   - lastStepIndex is max(0, len(contents)-1) (0 on Turn 1, 2 after 1 tool
//     call or 1 user follow-up, 4 after 2 tool calls, etc.)
//   - llmTurnCount is the count of prior model-generated turns (role=="model"
//     excluding functionResponse turns, which agy also tags with role=="model").
func CountAntigravityAgentSteps(payload []byte) (lastStepIndex int, llmTurnCount int) {
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		return 0, 0
	}
	total := 0
	contents.ForEach(func(_, content ojson.Result) bool {
		total++
		if !strings.EqualFold(content.Get("role").String(), "model") {
			return true
		}
		hasFunctionResponse := false
		parts := content.Get("parts")
		if parts.IsArray() {
			parts.ForEach(func(_, part ojson.Result) bool {
				if part.Get("functionResponse").Exists() || part.Get("function_response").Exists() {
					hasFunctionResponse = true
					return false
				}
				return true
			})
		}
		if !hasFunctionResponse {
			llmTurnCount++
		}
		return true
	})
	if llmTurnCount == 0 {
		// On Turn 1 (no prior model turns), agy always reports last_step_index=0
		// even if the client sent multiple leading user parts/turns.
		return 0, 0
	}
	if total > 0 {
		lastStepIndex = total - 1
	}
	return lastStepIndex, llmTurnCount
}

func antigravityFirstUserTargetText(parts ojson.Result) (partIdx int, text string) {
	partIdx = -1
	if !parts.IsArray() {
		return -1, ""
	}
	lastAnyIdx := -1
	lastAnyText := ""
	lastUserIdx := -1
	lastUserText := ""
	parts.ForEach(func(pi, part ojson.Result) bool {
		if part.Get("text").Type != ojson.String {
			return true
		}
		txt := part.Get("text").String()
		if txt == "" {
			return true
		}
		idx := int(pi.Int())
		lastAnyIdx = idx
		lastAnyText = txt
		trimmed := strings.TrimSpace(txt)
		if !strings.HasPrefix(trimmed, "<system-reminder>") && !strings.HasPrefix(trimmed, "<system-instructions>") {
			lastUserIdx = idx
			lastUserText = txt
		}
		return true
	})
	if lastUserIdx >= 0 {
		return lastUserIdx, lastUserText
	}
	return lastAnyIdx, lastAnyText
}

// AntigravityRootFingerprint extracts a stable root fingerprint from the full
// system instruction (SHA-256) and first user turn for fallback conversation
// tracking. It inspects only the first user turn (matching WrapAntigravityInitialUserRequest's
// text part selection and falling back to raw JSON for media-only turns) so
// multimodal and multi-part Turn 1s stay stable across Turn 2+.
func AntigravityRootFingerprint(payload []byte) string {
	var sb strings.Builder
	if sys := util.GetGJSONBytesNoCopy(payload, "request.systemInstruction.parts.0.text").String(); sys != "" {
		sum := sha256.Sum256([]byte(sys))
		sb.WriteString(hex.EncodeToString(sum[:16]))
		sb.WriteByte(0)
	}
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if contents.IsArray() {
		contents.ForEach(func(_, content ojson.Result) bool {
			if !strings.EqualFold(content.Get("role").String(), "user") {
				return true
			}
			_, targetText := antigravityFirstUserTargetText(content.Get("parts"))
			if targetText != "" {
				sb.WriteString(targetText)
			} else if content.Raw != "" {
				sb.WriteString(AntigravityHashSessionID(content.Raw))
			}
			return false
		})
	}
	return sb.String()
}

// --- Per-credential account state & Mendel AICode prediction routing ---

// AntigravityAccountState stores control-plane state discovered for an account.
type AntigravityAccountState struct {
	PaidTierID          string
	RegionCode          string
	TelemetryEnabled    *bool
	UseAICodeMendel     bool
	ControlPlaneSynced  bool
	LastCompanionSync   time.Time
	AICodeCooldownUntil time.Time
	LastPreSessionCheck time.Time
	UpdatedAt           time.Time
}

var (
	antigravityAcctMu         sync.RWMutex
	antigravityAcctState      = make(map[string]*AntigravityAccountState)
	antigravityControlPlaneSF singleflight.Group
)

func pruneAntigravityAccountStateLocked(now time.Time) {
	if len(antigravityAcctState) < antigravityAccountStateCapacity {
		return
	}
	cutoff := now.Add(-24 * time.Hour)
	for k, st := range antigravityAcctState {
		if st == nil || st.UpdatedAt.Before(cutoff) {
			delete(antigravityAcctState, k)
		}
	}
	if len(antigravityAcctState) >= antigravityAccountStateCapacity {
		clear(antigravityAcctState)
	}
}

// UpdateAntigravityAccountState merges control-plane metadata for credScope.
func UpdateAntigravityAccountState(credScope, paidTierID, regionCode string, telemetryEnabled *bool, useAICodeMendel *bool) {
	credScope = strings.TrimSpace(credScope)
	if credScope == "" {
		return
	}
	antigravityAcctMu.Lock()
	defer antigravityAcctMu.Unlock()
	now := time.Now()
	st, ok := antigravityAcctState[credScope]
	if !ok || st == nil {
		pruneAntigravityAccountStateLocked(now)
		st = &AntigravityAccountState{}
		antigravityAcctState[credScope] = st
	}
	if tier := strings.TrimSpace(paidTierID); tier != "" {
		st.PaidTierID = tier
	}
	if rc := strings.TrimSpace(regionCode); rc != "" {
		st.RegionCode = rc
	}
	if telemetryEnabled != nil {
		v := *telemetryEnabled
		st.TelemetryEnabled = &v
	}
	if useAICodeMendel != nil {
		st.UseAICodeMendel = *useAICodeMendel
	}
	st.UpdatedAt = now
}

// HasAntigravityControlPlaneSynced reports whether credScope has completed a
// full startup/warmup companion control-plane sync since process start.
func HasAntigravityControlPlaneSynced(credScope string) bool {
	st, ok := GetAntigravityAccountState(credScope)
	return ok && st.ControlPlaneSynced
}

// MarkAntigravityControlPlaneSynced records that credScope completed a full
// companion control-plane sync.
func MarkAntigravityControlPlaneSynced(credScope string) {
	credScope = strings.TrimSpace(credScope)
	if credScope == "" {
		return
	}
	antigravityAcctMu.Lock()
	defer antigravityAcctMu.Unlock()
	now := time.Now()
	st, ok := antigravityAcctState[credScope]
	if !ok || st == nil {
		pruneAntigravityAccountStateLocked(now)
		st = &AntigravityAccountState{}
		antigravityAcctState[credScope] = st
	}
	st.ControlPlaneSynced = true
	st.LastCompanionSync = now
	st.UpdatedAt = now
}

// HasRecentAntigravityCompanionSync reports whether credScope completed a full
// companion sync within minInterval.
func HasRecentAntigravityCompanionSync(credScope string, minInterval time.Duration) bool {
	st, ok := GetAntigravityAccountState(credScope)
	if !ok || !st.ControlPlaneSynced || st.LastCompanionSync.IsZero() {
		return false
	}
	if minInterval <= 0 {
		return true
	}
	return time.Since(st.LastCompanionSync) < minInterval
}

// DoAntigravityControlPlaneSync deduplicates startup/warmup/periodic companion
// control-plane bursts per credScope across both model probing and cold-start
// executor warmup. If a complete sync already succeeded within minInterval
// (or at all when minInterval <= 0), fn is skipped. Otherwise at most one fn
// runs at a time per credScope, and ControlPlaneSynced + LastCompanionSync are
// stamped only if fn returns true without context cancellation.
func DoAntigravityControlPlaneSync(ctx context.Context, credScope string, minInterval time.Duration, fn func(context.Context) bool) error {
	credScope = strings.TrimSpace(credScope)
	if credScope == "" || fn == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return errCtx
	}
	if HasRecentAntigravityCompanionSync(credScope, minInterval) {
		return nil
	}
	ch := antigravityControlPlaneSF.DoChan(credScope, func() (any, error) {
		if HasRecentAntigravityCompanionSync(credScope, minInterval) {
			return nil, nil
		}
		syncCtx := context.WithoutCancel(ctx)
		if ok := fn(syncCtx); ok && syncCtx.Err() == nil {
			MarkAntigravityControlPlaneSynced(credScope)
		}
		return nil, nil
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
		return ctx.Err()
	}
}

// GetAntigravityAccountState returns a snapshot of the stored state for credScope.
func GetAntigravityAccountState(credScope string) (AntigravityAccountState, bool) {
	credScope = strings.TrimSpace(credScope)
	if credScope == "" {
		return AntigravityAccountState{}, false
	}
	antigravityAcctMu.RLock()
	st, ok := antigravityAcctState[credScope]
	if !ok || st == nil {
		antigravityAcctMu.RUnlock()
		return AntigravityAccountState{}, false
	}
	cp := *st
	if st.TelemetryEnabled != nil {
		v := *st.TelemetryEnabled
		cp.TelemetryEnabled = &v
	}
	antigravityAcctMu.RUnlock()
	return cp, true
}

// ClaimAntigravityPreSessionCheck throttles background pre-session checks to
// at most once per interval per credential.
func ClaimAntigravityPreSessionCheck(credScope string, minInterval time.Duration) bool {
	credScope = strings.TrimSpace(credScope)
	if credScope == "" {
		return false
	}
	antigravityAcctMu.Lock()
	defer antigravityAcctMu.Unlock()
	now := time.Now()
	st, ok := antigravityAcctState[credScope]
	if !ok || st == nil {
		pruneAntigravityAccountStateLocked(now)
		st = &AntigravityAccountState{}
		antigravityAcctState[credScope] = st
	}
	if !st.LastPreSessionCheck.IsZero() && now.Sub(st.LastPreSessionCheck) < minInterval {
		return false
	}
	st.LastPreSessionCheck = now
	st.UpdatedAt = now
	return true
}

// ShouldUseAntigravityAICode reports whether consumer inference should route to
// https://aicode.googleapis.com/v1alpha:streamGenerateContent?alt=sse (matching
// agy 1.5.16's useAICodePredictionFlow, which defaults to false for consumer CLI
// traffic unless USE_AICODE=true is explicitly set) and returns the entitlement
// userTier to include in the AICode request.
func ShouldUseAntigravityAICode(credScope string) (string, bool) {
	st, _ := GetAntigravityAccountState(credScope)
	if !st.AICodeCooldownUntil.IsZero() && time.Now().Before(st.AICodeCooldownUntil) {
		return "", false
	}
	rawEnv := strings.TrimSpace(os.Getenv("USE_AICODE"))
	if rawEnv == "" {
		return "", false
	}
	useAICode, err := strconv.ParseBool(rawEnv)
	if err != nil || !useAICode || strings.TrimSpace(st.PaidTierID) == "" {
		return "", false
	}
	return st.PaidTierID, true
}

// MarkAntigravityAICodeCooldown temporarily falls back from aicode.googleapis.com
// to daily-cloudcode-pa after a scope/auth/location failure, matching agy's
// isInAICodeScopeCooldown behavior.
func MarkAntigravityAICodeCooldown(credScope string, duration time.Duration) {
	credScope = strings.TrimSpace(credScope)
	if credScope == "" {
		return
	}
	if duration <= 0 {
		duration = antigravityAICodeDefaultCooldown
	}
	antigravityAcctMu.Lock()
	defer antigravityAcctMu.Unlock()
	now := time.Now()
	st, ok := antigravityAcctState[credScope]
	if !ok || st == nil {
		pruneAntigravityAccountStateLocked(now)
		st = &AntigravityAccountState{}
		antigravityAcctState[credScope] = st
	}
	st.AICodeCooldownUntil = now.Add(duration)
	st.UpdatedAt = now
}

// antigravityProtoJSONCommaSep matches the compact protojson element separator
// emitted by agy 1.5.16, where protobuf/v2 detrand.init.0 parses the build CL
// string (1028571191 is odd -> bit 0 = 1 -> ", " on all platforms; an even CL
// flips to ","), while json.(*Encoder).WriteName writes ":" with no space.
var antigravityProtoJSONCommaSep = misc.AntigravityProtoJSONCommaSep()

type protoJSONWriteOptions struct {
	useEnumNumbers bool
}

// protoJSONObjectSpec describes the exact .proto field declaration order
// (protojson order.IndexNameFieldOrder) for a protobuf message and its nested
// message/map fields so outbound Antigravity wire JSON matches agy's protojson
// serializer recursively without HTML escaping.
type protoJSONObjectSpec struct {
	order                        []string
	fieldSpecs                   map[string]*protoJSONObjectSpec
	mapValueSpecs                map[string]*protoJSONObjectSpec
	enumFields                   map[string]map[string]int
	int64Fields                  map[string]struct{}
	encodeBypassThoughtSignature bool
}

var (
	antigravityCCPAProtoSpec          *protoJSONObjectSpec
	antigravityAICodeProtoSpec        *protoJSONObjectSpec
	antigravityControlPlaneProtoSpecs map[string]*protoJSONObjectSpec

	antigravitySchemaInt64Fields = map[string]struct{}{
		"minItems":      {},
		"maxItems":      {},
		"minProperties": {},
		"maxProperties": {},
		"minLength":     {},
		"maxLength":     {},
	}

	antigravitySchemaTypeEnums = map[string]int{
		"TYPE_UNSPECIFIED": 0,
		"STRING":           1,
		"NUMBER":           2,
		"INTEGER":          3,
		"BOOLEAN":          4,
		"ARRAY":            5,
		"OBJECT":           6,
		"NULL":             7,
	}
	antigravityFunctionDeclBehaviorEnums = map[string]int{
		"UNSPECIFIED":  0,
		"BLOCKING":     1,
		"NON_BLOCKING": 2,
	}
	antigravityFunctionRespSchedulingEnums = map[string]int{
		"SCHEDULING_UNSPECIFIED": 0,
		"SILENT":                 1,
		"WHEN_IDLE":              2,
		"INTERRUPT":              3,
	}
	antigravityMediaProcessingEnums = map[string]int{
		"MEDIA_PROCESSING_UNSPECIFIED": 0,
		"STATIC":                       1,
		"AGENTIC":                      2,
		"AUTO":                         3,
	}
	antigravityMediaResolutionLevelEnums = map[string]int{
		"MEDIA_RESOLUTION_UNSPECIFIED": 0,
		"MEDIA_RESOLUTION_LOW":         1,
		"MEDIA_RESOLUTION_MEDIUM":      2,
		"MEDIA_RESOLUTION_HIGH":        3,
		"MEDIA_RESOLUTION_ULTRA_HIGH":  4,
	}
	antigravityVideoContentLayerEnums = map[string]int{
		"CONTENT_LAYER_UNSPECIFIED": 0,
		"FRAMES":                    1,
		"AUDIO":                     2,
		"TRANSCRIPT":                3,
	}
	antigravityExecutableCodeLanguageEnums = map[string]int{
		"LANGUAGE_UNSPECIFIED": 0,
		"PYTHON":               1,
		"BASH":                 2,
	}
	antigravityCodeExecutionOutcomeEnums = map[string]int{
		"OUTCOME_UNSPECIFIED":       0,
		"OUTCOME_OK":                1,
		"OUTCOME_FAILED":            2,
		"OUTCOME_DEADLINE_EXCEEDED": 3,
	}
	antigravityFunctionCallingModeEnums = map[string]int{
		"MODE_UNSPECIFIED": 0,
		"AUTO":             1,
		"ANY":              2,
		"NONE":             3,
		"COMPOSITIONAL":    4,
		"VALIDATED":        5,
		"TEST":             6,
	}
	antigravityPhishBlockThresholdEnums = map[string]int{
		"PHISH_BLOCK_THRESHOLD_UNSPECIFIED": 0,
		"BLOCK_LOW_AND_ABOVE":               30,
		"BLOCK_MEDIUM_AND_ABOVE":            40,
		"BLOCK_HIGH_AND_ABOVE":              50,
		"BLOCK_HIGHER_AND_ABOVE":            55,
		"BLOCK_VERY_HIGH_AND_ABOVE":         60,
		"BLOCK_ONLY_EXTREMELY_HIGH":         100,
	}
	antigravityHarmCategoryEnums = map[string]int{
		"HARM_CATEGORY_UNSPECIFIED":             0,
		"HARM_CATEGORY_HATE_SPEECH":             1,
		"HARM_CATEGORY_DANGEROUS_CONTENT":       2,
		"HARM_CATEGORY_HARASSMENT":              3,
		"HARM_CATEGORY_SEXUALLY_EXPLICIT":       4,
		"HARM_CATEGORY_CIVIC_INTEGRITY":         5,
		"HARM_CATEGORY_IMAGE_HATE":              6,
		"HARM_CATEGORY_IMAGE_DANGEROUS_CONTENT": 7,
		"HARM_CATEGORY_IMAGE_HARASSMENT":        8,
		"HARM_CATEGORY_IMAGE_SEXUALLY_EXPLICIT": 9,
		"HARM_CATEGORY_JAILBREAK":               10,
	}
	antigravityHarmBlockThresholdEnums = map[string]int{
		"HARM_BLOCK_THRESHOLD_UNSPECIFIED": 0,
		"BLOCK_LOW_AND_ABOVE":              1,
		"BLOCK_MEDIUM_AND_ABOVE":           2,
		"BLOCK_ONLY_HIGH":                  3,
		"BLOCK_NONE":                       4,
		"OFF":                              5,
	}
	antigravityHarmBlockMethodEnums = map[string]int{
		"HARM_BLOCK_METHOD_UNSPECIFIED": 0,
		"SEVERITY":                      1,
		"PROBABILITY":                   2,
	}
	antigravityResponseStyleEnums = map[string]int{
		"RESPONSE_STYLE_UNSPECIFIED": 0,
		"RESPONSE_STYLE_PRECISE":     1,
		"RESPONSE_STYLE_BALANCED":    2,
		"RESPONSE_STYLE_CREATIVE":    3,
	}
	antigravityGenConfigModalityEnums = map[string]int{
		"MODALITY_UNSPECIFIED": 0,
		"TEXT":                 1,
		"IMAGE":                2,
		"AUDIO":                3,
		"VIDEO":                4,
	}
	antigravityGenConfigMediaResolutionEnums = map[string]int{
		"MEDIA_RESOLUTION_UNSPECIFIED": 0,
		"MEDIA_RESOLUTION_LOW":         1,
		"MEDIA_RESOLUTION_MEDIUM":      2,
		"MEDIA_RESOLUTION_HIGH":        3,
	}
	antigravityThinkingLevelEnums = map[string]int{
		"THINKING_LEVEL_UNSPECIFIED": 0,
		"LOW":                        1,
		"MEDIUM":                     2,
		"HIGH":                       3,
		"MINIMAL":                    4,
		"EXTRA_HIGH":                 5,
		"MAX":                        6,
		"XHIGH":                      7,
	}
	antigravityCreditTypeEnums = map[string]int{
		"CREDIT_TYPE_UNSPECIFIED": 0,
		"GOOGLE_ONE_AI":           1,
	}
	antigravityPrefixMismatchBehaviorEnums = map[string]int{
		"PREFIX_MISMATCH_BEHAVIOR_UNSPECIFIED": 0,
		"ERROR":                                1,
		"DROP":                                 2,
	}
	antigravityImagePersonGenerationEnums = map[string]int{
		"PERSON_GENERATION_UNSPECIFIED": 0,
		"ALLOW_ALL":                     1,
		"ALLOW_ADULT":                   2,
		"ALLOW_NONE":                    3,
	}
	antigravityImageProminentPeopleEnums = map[string]int{
		"PROMINENT_PEOPLE_UNSPECIFIED": 0,
		"ALLOW_PROMINENT_PEOPLE":       1,
		"BLOCK_PROMINENT_PEOPLE":       2,
	}
	antigravityImageBackgroundEnums = map[string]int{
		"BACKGROUND_UNSPECIFIED": 0,
		"AUTO":                   1,
		"TRANSPARENT":            2,
		"OPAQUE":                 3,
	}
	antigravityImageTaskEnums = map[string]int{
		"TASK_UNSPECIFIED": 0,
		"UPSCALE":          1,
		"SVG_LAYERING":     2,
	}
	antigravitySpeechModeEnums = map[string]int{
		"MODE_UNSPECIFIED": 0,
		"VERBATIM":         1,
		"NATURAL":          2,
	}
	antigravitySpeechTextNormalizationEnums = map[string]int{
		"TEXT_NORMALIZATION_UNSPECIFIED": 0,
		"AUTO":                           1,
		"ENABLED":                        2,
		"DISABLED":                       3,
	}
)

func init() {
	antigravityCCPAProtoSpec = buildAntigravityCCPAProtoSpec()
	antigravityAICodeProtoSpec = buildAntigravityAICodeProtoSpec()
	antigravityControlPlaneProtoSpecs = buildAntigravityControlPlaneProtoSpecs()
}

func buildAntigravityControlPlaneProtoSpecs() map[string]*protoJSONObjectSpec {
	clientMetadataSpec := &protoJSONObjectSpec{
		order: []string{
			"ideType",
			"ideVersion",
			"pluginVersion",
			"platform",
			"updateChannel",
			"duetProject",
			"pluginType",
			"ideName",
		},
	}
	projectSpec := &protoJSONObjectSpec{order: []string{"project"}}
	return map[string]*protoJSONObjectSpec{
		"loadCodeAssist": {
			order:      []string{"cloudaicompanionProject", "metadata", "mode"},
			fieldSpecs: map[string]*protoJSONObjectSpec{"metadata": clientMetadataSpec},
		},
		"onboardUser": {
			order:      []string{"tierId", "cloudaicompanionProject", "metadata"},
			fieldSpecs: map[string]*protoJSONObjectSpec{"metadata": clientMetadataSpec},
		},
		"fetchAvailableModels": {
			order: []string{"project", "requestId", "entitlement", "location"},
			fieldSpecs: map[string]*protoJSONObjectSpec{
				"entitlement": {order: []string{"userTier"}},
			},
		},
		"retrieveUserQuotaSummary": projectSpec,
		"fetchAdminControls":       projectSpec,
		"fetchUserInfo":            projectSpec,
		"writeTrajectoryAcls": {
			order: []string{"trajectoryId", "attributedOwner"},
		},
		"listExperiments": {
			order:      []string{"project", "metadata"},
			fieldSpecs: map[string]*protoJSONObjectSpec{"metadata": clientMetadataSpec},
		},
		"setUserSettings": {
			order: []string{"userSettings"},
			fieldSpecs: map[string]*protoJSONObjectSpec{
				"userSettings": {
					order: []string{
						"telemetryEnabled",
						"userDataCollectionForceDisabled",
						"marketingEmailsEnabled",
						"consumerTermsAccepted",
						"gcpTermsAccepted",
					},
				},
			},
		},
	}
}

// buildAntigravityCCPAProtoSpec constructs the exact .proto declaration order
// for google.internal.cloud.code.v1internal.GenerateContentRequest wrapping
// google.cloud.aiplatform.master.GenerateContentRequest.
func buildAntigravityCCPAProtoSpec() *protoJSONObjectSpec {
	schemaSpec := &protoJSONObjectSpec{
		order: []string{
			"type",
			"format",
			"title",
			"description",
			"nullable",
			"default",
			"items",
			"prefixItems",
			"minItems",
			"maxItems",
			"enum",
			"properties",
			"propertyOrdering",
			"required",
			"minProperties",
			"maxProperties",
			"minimum",
			"maximum",
			"minLength",
			"maxLength",
			"pattern",
			"example",
			"oneOf",
			"anyOf",
			"allOf",
			"not",
			"additionalProperties",
			"additionalPropertiesSchema",
			"ref",
			"defs",
		},
		enumFields: map[string]map[string]int{
			"type": antigravitySchemaTypeEnums,
		},
		int64Fields: antigravitySchemaInt64Fields,
	}
	schemaSpec.fieldSpecs = map[string]*protoJSONObjectSpec{
		"items":                      schemaSpec,
		"prefixItems":                schemaSpec,
		"oneOf":                      schemaSpec,
		"anyOf":                      schemaSpec,
		"allOf":                      schemaSpec,
		"not":                        schemaSpec,
		"additionalPropertiesSchema": schemaSpec,
	}
	schemaSpec.mapValueSpecs = map[string]*protoJSONObjectSpec{
		"properties": schemaSpec,
		"defs":       schemaSpec,
	}

	blobSpec := &protoJSONObjectSpec{
		order: []string{"mimeType", "data", "willContinue", "displayName", "toolInputOnly"},
	}
	fileDataSpec := &protoJSONObjectSpec{
		order: []string{"mimeType", "fileUri", "displayName", "toolInputOnly"},
	}
	videoMetadataSpec := &protoJSONObjectSpec{
		order: []string{"startOffset", "endOffset", "fps", "contentLayers"},
		enumFields: map[string]map[string]int{
			"contentLayers": antigravityVideoContentLayerEnums,
		},
	}
	mediaResolutionSpec := &protoJSONObjectSpec{
		order: []string{"level"},
		enumFields: map[string]map[string]int{
			"level": antigravityMediaResolutionLevelEnums,
		},
	}
	funcCallSpec := &protoJSONObjectSpec{
		order: []string{"id", "name", "args", "partialArgs", "willContinue"},
	}
	funcRespPartSpec := &protoJSONObjectSpec{
		order: []string{
			"inlineData",
			"fileData",
			"retrievalResult",
			"mediaResolution",
			"videoMetadata",
			"documentMetadata",
			"spreadsheetMetadata",
			"audioMetadata",
			"mediaProcessing",
		},
		fieldSpecs: map[string]*protoJSONObjectSpec{
			"inlineData":      {order: []string{"mimeType", "data", "displayName"}},
			"fileData":        {order: []string{"mimeType", "fileUri", "displayName"}},
			"mediaResolution": mediaResolutionSpec,
			"videoMetadata":   videoMetadataSpec,
		},
		enumFields: map[string]map[string]int{
			"mediaProcessing": antigravityMediaProcessingEnums,
		},
	}
	funcRespSpec := &protoJSONObjectSpec{
		order: []string{"id", "name", "response", "parts", "scheduling"},
		fieldSpecs: map[string]*protoJSONObjectSpec{
			"parts": funcRespPartSpec,
		},
		enumFields: map[string]map[string]int{
			"scheduling": antigravityFunctionRespSchedulingEnums,
		},
	}
	funcDeclSpec := &protoJSONObjectSpec{
		order: []string{
			"name",
			"description",
			"parameters",
			"parametersJsonSchema",
			"response",
			"responseJsonSchema",
			"enableResponseGrounding",
			"behavior",
			"deferLoading",
			"shortDescription",
		},
		fieldSpecs: map[string]*protoJSONObjectSpec{
			"parameters": schemaSpec,
			"response":   schemaSpec,
		},
		enumFields: map[string]map[string]int{
			"behavior": antigravityFunctionDeclBehaviorEnums,
		},
	}
	partSpec := &protoJSONObjectSpec{
		order: []string{
			"text",
			"inlineData",
			"fileData",
			"functionCall",
			"functionResponse",
			"executableCode",
			"codeExecutionResult",
			"preprocessInputs",
			"toolCall",
			"toolResponse",
			"functionDeclaration",
			"functionRemoval",
			"speechMetadata",
			"imageMetadata",
			"thought",
			"thoughtSignature",
			"mediaResolution",
			"mediaProcessing",
			"videoMetadata",
			"customMetadata",
			"documentMetadata",
			"spreadsheetMetadata",
			"audioMetadata",
			"internalSettings",
			"sourceAnnotations",
			"audioTranscription",
			"rawThought",
		},
		fieldSpecs: map[string]*protoJSONObjectSpec{
			"inlineData":       blobSpec,
			"fileData":         fileDataSpec,
			"functionCall":     funcCallSpec,
			"functionResponse": funcRespSpec,
			"executableCode": {
				order: []string{"language", "code", "id"},
				enumFields: map[string]map[string]int{
					"language": antigravityExecutableCodeLanguageEnums,
				},
			},
			"codeExecutionResult": {
				order: []string{"outcome", "output", "id"},
				enumFields: map[string]map[string]int{
					"outcome": antigravityCodeExecutionOutcomeEnums,
				},
			},
			"functionDeclaration": funcDeclSpec,
			"mediaResolution":     mediaResolutionSpec,
			"videoMetadata":       videoMetadataSpec,
		},
		enumFields: map[string]map[string]int{
			"mediaProcessing": antigravityMediaProcessingEnums,
		},
		encodeBypassThoughtSignature: true,
	}
	contentSpec := &protoJSONObjectSpec{
		order: []string{"role", "parts"},
		fieldSpecs: map[string]*protoJSONObjectSpec{
			"parts": partSpec,
		},
	}
	toolSpec := &protoJSONObjectSpec{
		order: []string{
			"functionDeclarations",
			"retrieval",
			"googleSearch",
			"imageSearch",
			"googleSearchRetrieval",
			"googleMaps",
			"enterpriseWebSearch",
			"parallelAiSearch",
			"exaAiSearch",
			"codeExecution",
			"urlContext",
			"computerUse",
			"bash",
			"toolSearch",
		},
		fieldSpecs: map[string]*protoJSONObjectSpec{
			"functionDeclarations": funcDeclSpec,
			"googleSearch": {
				order: []string{
					"timeRangeFilter",
					"enhancedContent",
					"excludeDomains",
					"blockingConfidence",
					"includedDomains",
					"searchTypes",
					"excludedDomainsSource",
				},
				fieldSpecs: map[string]*protoJSONObjectSpec{
					"timeRangeFilter": {order: []string{"startTime", "endTime"}},
				},
				enumFields: map[string]map[string]int{
					"blockingConfidence": antigravityPhishBlockThresholdEnums,
				},
			},
			"codeExecution": {
				order: []string{"languages"},
				enumFields: map[string]map[string]int{
					"languages": antigravityExecutableCodeLanguageEnums,
				},
			},
		},
	}
	toolConfigSpec := &protoJSONObjectSpec{
		order: []string{
			"functionCallingConfig",
			"retrievalConfig",
			"toolTurnLimit",
			"includeServerSideToolInvocations",
		},
		fieldSpecs: map[string]*protoJSONObjectSpec{
			"functionCallingConfig": {
				order: []string{
					"mode",
					"allowedFunctionNames",
					"parallelFunctionCallConfig",
					"streamFunctionCallArguments",
				},
				enumFields: map[string]map[string]int{
					"mode": antigravityFunctionCallingModeEnums,
				},
			},
			"retrievalConfig": {order: []string{"latLng", "languageCode", "timeZone"}},
		},
	}
	safetySettingSpec := &protoJSONObjectSpec{
		order: []string{"category", "threshold", "method"},
		enumFields: map[string]map[string]int{
			"category":  antigravityHarmCategoryEnums,
			"threshold": antigravityHarmBlockThresholdEnums,
			"method":    antigravityHarmBlockMethodEnums,
		},
	}
	speechConfigSpec := &protoJSONObjectSpec{
		order: []string{"voiceConfig", "languageCode", "multiSpeakerVoiceConfig", "textNormalization", "mode", "speed"},
		fieldSpecs: map[string]*protoJSONObjectSpec{
			"voiceConfig": {
				order: []string{"prebuiltVoiceConfig", "customVoiceConfig", "replicatedVoiceConfig", "voice"},
				fieldSpecs: map[string]*protoJSONObjectSpec{
					"prebuiltVoiceConfig": {order: []string{"voiceName"}},
				},
			},
		},
		enumFields: map[string]map[string]int{
			"textNormalization": antigravitySpeechTextNormalizationEnums,
			"mode":              antigravitySpeechModeEnums,
		},
	}
	generationConfigSpec := &protoJSONObjectSpec{
		order: []string{
			"temperature",
			"topP",
			"topK",
			"candidateCount",
			"maxOutputTokens",
			"stopSequences",
			"responseLogprobs",
			"logprobs",
			"requestLogprobs",
			"requestTopKLogprobs",
			"requestLogprobsConfig",
			"presencePenalty",
			"frequencyPenalty",
			"logitBias",
			"echo",
			"seed",
			"responseMimeType",
			"responseSchema",
			"responseJsonSchema",
			"responseRegexConstraint",
			"responseStyle",
			"routingConfig",
			"minP",
			"audioTimestamp",
			"responseModalities",
			"mediaResolution",
			"speechConfig",
			"contextConfig",
			"thinkingConfig",
			"modelConfig",
			"enableAffectiveDialog",
			"imageConfig",
			"videoConfig",
			"responseFormat",
			"audioTranscriptionConfig",
			"skipSummaryOnResponse",
			"translationConfig",
		},
		fieldSpecs: map[string]*protoJSONObjectSpec{
			"responseSchema": schemaSpec,
			"speechConfig":   speechConfigSpec,
			"thinkingConfig": {
				order: []string{
					"includeThoughts",
					"thinkingBudget",
					"thinkingLevel",
					"includeRawThoughts",
					"enableThinking",
					"prefixMismatchBehavior",
				},
				enumFields: map[string]map[string]int{
					"thinkingLevel":          antigravityThinkingLevelEnums,
					"prefixMismatchBehavior": antigravityPrefixMismatchBehaviorEnums,
				},
			},
			"imageConfig": {
				order: []string{
					"imageOutputOptions",
					"aspectRatio",
					"personGeneration",
					"imageSize",
					"prominentPeople",
					"background",
					"task",
				},
				fieldSpecs: map[string]*protoJSONObjectSpec{
					"imageOutputOptions": {order: []string{"mimeType", "compressionQuality"}},
				},
				enumFields: map[string]map[string]int{
					"personGeneration": antigravityImagePersonGenerationEnums,
					"prominentPeople":  antigravityImageProminentPeopleEnums,
					"background":       antigravityImageBackgroundEnums,
					"task":             antigravityImageTaskEnums,
				},
			},
		},
		enumFields: map[string]map[string]int{
			"responseStyle":      antigravityResponseStyleEnums,
			"responseModalities": antigravityGenConfigModalityEnums,
			"mediaResolution":    antigravityGenConfigMediaResolutionEnums,
		},
	}
	innerSpec := &protoJSONObjectSpec{
		order: []string{
			"endpoint",
			"model",
			"contents",
			"systemInstruction",
			"preambleConfig",
			"cachedContent",
			"tools",
			"toolConfig",
			"labels",
			"safetySettings",
			"modelArmorConfig",
			"generationConfig",
			"serviceAccount",
			"sessionId",
			"uniserveEndpointOverride",
			"globalEndpointTargetLocationHintTestingOnly",
			"serviceTier",
			"implicitCacheConfig",
			"admitTime",
			"failFast",
			"continuationToken",
			"outputConfig",
		},
		fieldSpecs: map[string]*protoJSONObjectSpec{
			"contents":          contentSpec,
			"systemInstruction": contentSpec,
			"tools":             toolSpec,
			"toolConfig":        toolConfigSpec,
			"safetySettings":    safetySettingSpec,
			"generationConfig":  generationConfigSpec,
			"outputConfig":      {order: []string{"outputUriPrefix"}},
		},
	}
	return &protoJSONObjectSpec{
		order: []string{
			"project",
			"requestId",
			"request",
			"model",
			"userPromptId",
			"userAgent",
			"requestType",
			"enabledCreditTypes",
		},
		fieldSpecs: map[string]*protoJSONObjectSpec{
			"request": innerSpec,
		},
		enumFields: map[string]map[string]int{
			"enabledCreditTypes": antigravityCreditTypeEnums,
		},
	}
}

// buildAntigravityAICodeProtoSpec constructs the exact .proto declaration order
// for google.gca.aicode.v1main.GenerateContentRequest.
// Note: agy 1.5.16's aicodeConverter/baicodeConverter always call protojson.Marshal
// with UseEnumNumbers=false (unknown thinkingLevel > 4 stays numeric while all
// other enums remain string names; aicode.v1main.HarmCategory also uses 7..10
// rather than aiplatform.master's 1..4), so enumFields are intentionally omitted.
func buildAntigravityAICodeProtoSpec() *protoJSONObjectSpec {
	schemaSpec := &protoJSONObjectSpec{
		order: []string{
			"type",
			"format",
			"description",
			"nullable",
			"enum",
			"items",
			"properties",
			"required",
			"minimum",
			"maximum",
			"minLength",
			"maxLength",
			"pattern",
			"example",
			"title",
			"default",
			"minItems",
			"maxItems",
			"minProperties",
			"maxProperties",
			"propertyOrdering",
			"anyOf",
		},
		int64Fields: antigravitySchemaInt64Fields,
	}
	schemaSpec.fieldSpecs = map[string]*protoJSONObjectSpec{
		"items": schemaSpec,
		"anyOf": schemaSpec,
	}
	schemaSpec.mapValueSpecs = map[string]*protoJSONObjectSpec{
		"properties": schemaSpec,
	}

	blobSpec := &protoJSONObjectSpec{order: []string{"mimeType", "data"}}
	fileDataSpec := &protoJSONObjectSpec{order: []string{"mimeType", "fileUri"}}
	partSpec := &protoJSONObjectSpec{
		order: []string{
			"text",
			"inlineData",
			"fileData",
			"functionCall",
			"functionResponse",
			"executableCode",
			"codeExecutionResult",
			"videoMetadata",
			"thought",
			"thoughtSignature",
		},
		fieldSpecs: map[string]*protoJSONObjectSpec{
			"inlineData":   blobSpec,
			"fileData":     fileDataSpec,
			"functionCall": {order: []string{"name", "args", "id"}},
			"functionResponse": {
				order: []string{"name", "response", "id", "parts"},
				fieldSpecs: map[string]*protoJSONObjectSpec{
					"parts": {
						order:      []string{"inlineData"},
						fieldSpecs: map[string]*protoJSONObjectSpec{"inlineData": blobSpec},
					},
				},
			},
			"executableCode":      {order: []string{"language", "code"}},
			"codeExecutionResult": {order: []string{"outcome", "output"}},
			"videoMetadata":       {order: []string{"startOffset", "endOffset", "fps"}},
		},
		encodeBypassThoughtSignature: true,
	}
	contentSpec := &protoJSONObjectSpec{
		order: []string{"parts", "role"},
		fieldSpecs: map[string]*protoJSONObjectSpec{
			"parts": partSpec,
		},
	}
	funcDeclSpec := &protoJSONObjectSpec{
		order: []string{
			"name",
			"description",
			"parameters",
			"parametersJsonSchema",
			"response",
			"responseJsonSchema",
		},
		fieldSpecs: map[string]*protoJSONObjectSpec{
			"parameters": schemaSpec,
			"response":   schemaSpec,
		},
	}
	toolSpec := &protoJSONObjectSpec{
		order: []string{"functionDeclarations", "googleSearch"},
		fieldSpecs: map[string]*protoJSONObjectSpec{
			"functionDeclarations": funcDeclSpec,
			"googleSearch": {
				order: []string{"timeRangeFilter"},
				fieldSpecs: map[string]*protoJSONObjectSpec{
					"timeRangeFilter": {order: []string{"startTime", "endTime"}},
				},
			},
		},
	}
	toolConfigSpec := &protoJSONObjectSpec{
		order: []string{"functionCallingConfig"},
		fieldSpecs: map[string]*protoJSONObjectSpec{
			"functionCallingConfig": {
				order: []string{"mode", "allowedFunctionNames"},
			},
		},
	}
	generationConfigSpec := &protoJSONObjectSpec{
		order: []string{
			"candidateCount",
			"stopSequences",
			"maxOutputTokens",
			"temperature",
			"topP",
			"topK",
			"seed",
			"responseMimeType",
			"responseSchema",
			"responseJsonSchema",
			"presencePenalty",
			"frequencyPenalty",
			"thinkingConfig",
			"responseLogprobs",
			"logprobs",
			"responseModalities",
			"mediaResolution",
		},
		fieldSpecs: map[string]*protoJSONObjectSpec{
			"responseSchema": schemaSpec,
			"thinkingConfig": {
				order: []string{"includeThoughts", "thinkingBudget", "thinkingLevel"},
			},
		},
	}
	return &protoJSONObjectSpec{
		order: []string{
			"model",
			"aicode",
			"contents",
			"systemInstruction",
			"tools",
			"toolConfig",
			"labels",
			"safetySettings",
			"generationConfig",
			"sessionId",
			"entitlement",
		},
		fieldSpecs: map[string]*protoJSONObjectSpec{
			"aicode":            {order: []string{"experience", "files", "additionalContent", "enabledCredits"}},
			"contents":          contentSpec,
			"systemInstruction": contentSpec,
			"tools":             toolSpec,
			"toolConfig":        toolConfigSpec,
			"safetySettings":    {order: []string{"category", "threshold", "method"}},
			"generationConfig":  generationConfigSpec,
			"entitlement":       {order: []string{"userTier"}},
		},
	}
}

// BuildAntigravityAICodePayload converts a normalized Cloud Code envelope
// ({"project", "requestId", "request": {...}, "model", ...}) into agy's
// google.gca.aicode.v1main.GenerateContentRequest JSON payload for
// POST https://aicode.googleapis.com/v1alpha:streamGenerateContent?alt=sse.
// Note: model and aicode are mutually exclusive members of oneof inference;
// setting model omits aicode so Google does not reject the request with 400.
func BuildAntigravityAICodePayload(ccpaPayload []byte, userTier string) ([]byte, error) {
	parsed := util.ParseGJSONBytesNoCopy(ccpaPayload)
	if !parsed.IsObject() {
		return nil, fmt.Errorf("aicode payload: invalid JSON object")
	}
	model := strings.TrimSpace(parsed.Get("model").String())
	inner := parsed.Get("request")
	if model == "" || !inner.IsObject() {
		return nil, fmt.Errorf("aicode payload: missing model or request object")
	}

	rawFields := make(map[string]json.RawMessage, 10)
	modelJSON, errModel := marshalAntigravityJSONNoEscape(model)
	if errModel != nil {
		return nil, errModel
	}
	rawFields["model"] = json.RawMessage(modelJSON)

	inner.ForEach(func(k, v ojson.Result) bool {
		rawVal := strings.TrimSpace(v.Raw)
		if rawVal == "" {
			return true
		}
		key := k.String()
		switch key {
		case "contents", "systemInstruction", "tools", "toolConfig", "labels", "safetySettings", "generationConfig", "sessionId":
			rawFields[key] = json.RawMessage(rawVal)
		case "tool_config":
			if _, exists := rawFields["toolConfig"]; !exists {
				rawFields["toolConfig"] = json.RawMessage(rawVal)
			}
		}
		return true
	})

	if tier := strings.TrimSpace(userTier); tier != "" {
		entJSON, errEnt := marshalAntigravityJSONNoEscape(map[string]string{"userTier": tier})
		if errEnt == nil {
			rawFields["entitlement"] = json.RawMessage(entJSON)
		}
	}

	var buf bytes.Buffer
	buf.Grow(len(ccpaPayload) + 128)
	// agy 1.5.16's aicodeConverter/baicodeConverter always call standard
	// protojson.Marshal with UseEnumNumbers=false: unknown thinkingLevel > 4
	// stays numeric while all other enums serialize as strings.
	opts := protoJSONWriteOptions{useEnumNumbers: false}
	if errWrite := writeProtoJSONObjectNoEscape(&buf, rawFields, antigravityAICodeProtoSpec, opts); errWrite != nil {
		return nil, errWrite
	}
	return buf.Bytes(), nil
}

func writeAntigravityGenericJSONValue(buf *bytes.Buffer, val any) error {
	switch typed := val.(type) {
	case nil:
		buf.WriteString("null")
		return nil
	case bool:
		if typed {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil
	case json.Number:
		buf.WriteString(typed.String())
		return nil
	case string:
		encoded, err := marshalAntigravityJSONNoEscape(typed)
		if err != nil {
			return err
		}
		buf.Write(encoded)
		return nil
	case []any:
		buf.WriteByte('[')
		for i, elem := range typed {
			if i > 0 {
				buf.WriteString(antigravityProtoJSONCommaSep)
			}
			if err := writeAntigravityGenericJSONValue(buf, elem); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for k := range typed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteString(antigravityProtoJSONCommaSep)
			}
			keyJSON, errKey := marshalAntigravityJSONNoEscape(k)
			if errKey != nil {
				return errKey
			}
			buf.Write(keyJSON)
			buf.WriteByte(':')
			if errVal := writeAntigravityGenericJSONValue(buf, typed[k]); errVal != nil {
				return errVal
			}
		}
		buf.WriteByte('}')
		return nil
	default:
		encoded, err := marshalAntigravityJSONNoEscape(typed)
		if err != nil {
			return err
		}
		buf.Write(encoded)
		return nil
	}
}

func writeReencodedJSONNoHTMLEscape(buf *bytes.Buffer, raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var val any
	if err := dec.Decode(&val); err != nil {
		buf.Write(raw)
		return nil
	}
	return writeAntigravityGenericJSONValue(buf, val)
}

func writeProtoJSONEnumField(buf *bytes.Buffer, raw json.RawMessage, enumMap map[string]int) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	switch trimmed[0] {
	case '"':
		var name string
		if err := json.Unmarshal(trimmed, &name); err != nil {
			return false
		}
		if num, ok := enumMap[strings.ToUpper(strings.TrimSpace(name))]; ok {
			buf.WriteString(strconv.Itoa(num))
			return true
		}
		return false
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return false
		}
		buf.WriteByte('[')
		for i, item := range items {
			if i > 0 {
				buf.WriteString(antigravityProtoJSONCommaSep)
			}
			if !writeProtoJSONEnumField(buf, item, enumMap) {
				_ = writeReencodedJSONNoHTMLEscape(buf, bytes.TrimSpace(item))
			}
		}
		buf.WriteByte(']')
		return true
	default:
		return false
	}
}

func writeProtoJSONInt64Field(buf *bytes.Buffer, raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] == '"' {
		return false
	}
	s := string(trimmed)
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		buf.WriteByte('"')
		buf.Write(trimmed)
		buf.WriteByte('"')
		return true
	}
	f, errFloat := strconv.ParseFloat(s, 64)
	if errFloat != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Trunc(f) != f {
		return false
	}
	if f < math.MinInt64 || f >= float64(math.MaxInt64) {
		return false
	}
	buf.WriteByte('"')
	buf.WriteString(strconv.FormatInt(int64(f), 10))
	buf.WriteByte('"')
	return true
}

func writeProtoJSONThoughtSignatureField(buf *bytes.Buffer, raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return false
	}
	var sig string
	if err := json.Unmarshal(trimmed, &sig); err != nil {
		return false
	}
	if !internalsignature.IsAntigravityThoughtSignatureBypass(sig) {
		return false
	}
	encoded, err := marshalAntigravityJSONNoEscape(internalsignature.GeminiSkipThoughtSignatureValidatorBase64)
	if err != nil {
		return false
	}
	buf.Write(encoded)
	return true
}

func writeProtoJSONValueNoEscape(buf *bytes.Buffer, raw json.RawMessage, spec *protoJSONObjectSpec, opts protoJSONWriteOptions) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return fmt.Errorf("protojson: empty JSON value")
	}
	if spec == nil {
		return writeReencodedJSONNoHTMLEscape(buf, trimmed)
	}

	switch trimmed[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return writeReencodedJSONNoHTMLEscape(buf, trimmed)
		}
		buf.WriteByte('[')
		for i, elem := range items {
			if i > 0 {
				buf.WriteString(antigravityProtoJSONCommaSep)
			}
			if errElem := writeProtoJSONValueNoEscape(buf, elem, spec, opts); errElem != nil {
				return errElem
			}
		}
		buf.WriteByte(']')
		return nil
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return writeReencodedJSONNoHTMLEscape(buf, trimmed)
		}
		return writeProtoJSONObjectNoEscape(buf, obj, spec, opts)
	default:
		return writeReencodedJSONNoHTMLEscape(buf, trimmed)
	}
}

func writeProtoJSONObjectNoEscape(buf *bytes.Buffer, obj map[string]json.RawMessage, spec *protoJSONObjectSpec, opts protoJSONWriteOptions) error {
	var order []string
	var fieldSpecs map[string]*protoJSONObjectSpec
	var mapValueSpecs map[string]*protoJSONObjectSpec
	var enumFields map[string]map[string]int
	var int64Fields map[string]struct{}
	encodeBypassSig := false
	if spec != nil {
		order = spec.order
		fieldSpecs = spec.fieldSpecs
		mapValueSpecs = spec.mapValueSpecs
		enumFields = spec.enumFields
		int64Fields = spec.int64Fields
		encodeBypassSig = spec.encodeBypassThoughtSignature
	}

	seen := make(map[string]struct{}, len(order))
	orderedKeys := make([]string, 0, len(obj))
	for _, k := range order {
		if _, ok := obj[k]; ok {
			orderedKeys = append(orderedKeys, k)
			seen[k] = struct{}{}
		}
	}
	var extra []string
	for k := range obj {
		if _, ok := seen[k]; !ok {
			extra = append(extra, k)
		}
	}
	if len(extra) > 1 {
		sort.Strings(extra)
	}
	orderedKeys = append(orderedKeys, extra...)

	buf.WriteByte('{')
	for i, k := range orderedKeys {
		if i > 0 {
			buf.WriteString(antigravityProtoJSONCommaSep)
		}
		keyJSON, errKey := marshalAntigravityJSONNoEscape(k)
		if errKey != nil {
			return errKey
		}
		buf.Write(keyJSON)
		buf.WriteByte(':')

		rawVal := obj[k]
		if encodeBypassSig && k == "thoughtSignature" && writeProtoJSONThoughtSignatureField(buf, rawVal) {
			continue
		}
		if opts.useEnumNumbers {
			if enumMap := enumFields[k]; enumMap != nil && writeProtoJSONEnumField(buf, rawVal, enumMap) {
				continue
			}
		}
		if _, isInt64 := int64Fields[k]; isInt64 && writeProtoJSONInt64Field(buf, rawVal) {
			continue
		}
		if childSpec := fieldSpecs[k]; childSpec != nil {
			if errChild := writeProtoJSONValueNoEscape(buf, rawVal, childSpec, opts); errChild != nil {
				return errChild
			}
			continue
		}
		if mapValSpec := mapValueSpecs[k]; mapValSpec != nil {
			if errMap := writeProtoJSONMapNoEscape(buf, rawVal, mapValSpec, opts); errMap != nil {
				return errMap
			}
			continue
		}
		if errLeaf := writeProtoJSONValueNoEscape(buf, rawVal, nil, opts); errLeaf != nil {
			return errLeaf
		}
	}
	buf.WriteByte('}')
	return nil
}

// writeProtoJSONMapNoEscape serializes a protobuf map<string, Message> object:
// keys are sorted lexicographically (protojson order.GenericKeyOrder) and each
// map value is serialized using valSpec.
func writeProtoJSONMapNoEscape(buf *bytes.Buffer, raw json.RawMessage, valSpec *protoJSONObjectSpec, opts protoJSONWriteOptions) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return writeProtoJSONValueNoEscape(buf, raw, nil, opts)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return writeProtoJSONValueNoEscape(buf, raw, nil, opts)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteString(antigravityProtoJSONCommaSep)
		}
		keyJSON, errKey := marshalAntigravityJSONNoEscape(k)
		if errKey != nil {
			return errKey
		}
		buf.Write(keyJSON)
		buf.WriteByte(':')
		if errVal := writeProtoJSONValueNoEscape(buf, obj[k], valSpec, opts); errVal != nil {
			return errVal
		}
	}
	buf.WriteByte('}')
	return nil
}

// antigravityCCPAShouldUseEnumNumbers mirrors agy 1.5.16's ccpahttp.MarshalRequest:
// whenever thinkingLevel > 4 (or string EXTRA_HIGH/MAX/XHIGH), protojson switches
// on UseEnumNumbers=true across the entire outer v1internal.GenerateContentRequest
// (including enabledCreditTypes) so the payload never mixes numeric and string
// enum encodings. Numeric thinkingLevel values 0..4 keep UseEnumNumbers=false.
func antigravityCCPAShouldUseEnumNumbers(innerRequestRaw json.RawMessage) bool {
	if len(innerRequestRaw) == 0 {
		return false
	}
	lvl := ojson.GetBytes(innerRequestRaw, "generationConfig.thinkingConfig.thinkingLevel")
	if !lvl.Exists() {
		return false
	}
	switch lvl.Type {
	case ojson.Number:
		return lvl.Int() > 4
	case ojson.String:
		switch strings.ToUpper(strings.TrimSpace(lvl.String())) {
		case "EXTRA_HIGH", "MAX", "XHIGH":
			return true
		}
	}
	return false
}

// FormatAntigravityControlPlaneJSON serializes an outbound Cloud Code v1internal
// control-plane JSON object body in agy 1.5.16's exact .proto declaration order
// with detrand ", " spacing and disabled HTML escaping. For inference RPCs
// (streamGenerateContent / generateContent), it delegates to
// FormatAntigravityCCPAWireJSON.
func FormatAntigravityControlPlaneJSON(action string, payload []byte) []byte {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return payload
	}
	switch strings.TrimSpace(action) {
	case "streamGenerateContent", "generateContent":
		return FormatAntigravityCCPAWireJSON(trimmed)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return payload
	}
	if len(obj) == 0 {
		return []byte("{}")
	}
	spec := antigravityControlPlaneProtoSpecs[strings.TrimSpace(action)]
	var buf bytes.Buffer
	buf.Grow(len(trimmed) + 32)
	if err := writeProtoJSONObjectNoEscape(&buf, obj, spec, protoJSONWriteOptions{}); err != nil || buf.Len() == 0 {
		return payload
	}
	return buf.Bytes()
}

// FormatAntigravityCCPAWireJSON serializes an outbound Cloud Code request body
// in agy 1.5.16's exact protojson declaration order across the outer envelope,
// inner request, and all nested protobuf messages (Content, Part, Tool,
// FunctionDeclaration, recursive Schema, FunctionCall, FunctionResponse,
// ToolConfig, SafetySetting, GenerationConfig, ThinkingConfig) with agy 1.5.16's
// detrand ", " spacing, UseEnumNumbers consistency for xhigh/thinkingLevel > 4,
// protobuf int64 string encoding on Schema bounds, base64 thoughtSignature
// bypass sentinels, and disabled HTML escaping.
func FormatAntigravityCCPAWireJSON(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(payload, &outer); err != nil || len(outer) == 0 {
		return payload
	}
	opts := protoJSONWriteOptions{
		useEnumNumbers: antigravityCCPAShouldUseEnumNumbers(outer["request"]),
	}
	var buf bytes.Buffer
	buf.Grow(len(payload) + len(payload)/16)
	if err := writeProtoJSONObjectNoEscape(&buf, outer, antigravityCCPAProtoSpec, opts); err != nil || buf.Len() == 0 {
		return payload
	}
	return buf.Bytes()
}

// CanonicalizeAntigravityProtoKeys converts snake_case Gemini protobuf field
// names inside request.tools and request.contents (such as function_declarations,
// function_call, function_response, inline_data, file_data, mime_type, file_uri,
// and thought_signature) to agy's canonical protojson camelCase without touching
// arbitrary tool arguments or functionResponse payloads.
func CanonicalizeAntigravityProtoKeys(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	if !bytes.Contains(payload, []byte(`"function_declarations"`)) &&
		!bytes.Contains(payload, []byte(`"function_call"`)) &&
		!bytes.Contains(payload, []byte(`"function_response"`)) &&
		!bytes.Contains(payload, []byte(`"inline_data"`)) &&
		!bytes.Contains(payload, []byte(`"file_data"`)) &&
		!bytes.Contains(payload, []byte(`"mime_type"`)) &&
		!bytes.Contains(payload, []byte(`"file_uri"`)) &&
		!bytes.Contains(payload, []byte(`"thought_signature"`)) {
		return payload
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil || root == nil {
		return payload
	}
	reqObj, ok := root["request"].(map[string]any)
	if !ok || reqObj == nil {
		return payload
	}
	changed := false
	if tools, okTools := reqObj["tools"].([]any); okTools {
		for _, item := range tools {
			toolMap, okMap := item.(map[string]any)
			if !okMap {
				continue
			}
			if renameMapKeyIfAbsent(toolMap, "function_declarations", "functionDeclarations") {
				changed = true
			}
		}
	}
	if contents, okContents := reqObj["contents"].([]any); okContents {
		for _, item := range contents {
			contentMap, okMap := item.(map[string]any)
			if !okMap {
				continue
			}
			parts, okParts := contentMap["parts"].([]any)
			if !okParts {
				continue
			}
			for _, p := range parts {
				partMap, okPart := p.(map[string]any)
				if !okPart {
					continue
				}
				if renameMapKeyIfAbsent(partMap, "function_call", "functionCall") {
					changed = true
				}
				if renameMapKeyIfAbsent(partMap, "function_response", "functionResponse") {
					changed = true
				}
				if renameMapKeyIfAbsent(partMap, "inline_data", "inlineData") {
					changed = true
				}
				if renameMapKeyIfAbsent(partMap, "file_data", "fileData") {
					changed = true
				}
				if renameMapKeyIfAbsent(partMap, "thought_signature", "thoughtSignature") {
					changed = true
				}
				if inlineObj, okInline := partMap["inlineData"].(map[string]any); okInline {
					if renameMapKeyIfAbsent(inlineObj, "mime_type", "mimeType") {
						changed = true
					}
				}
				if fileObj, okFile := partMap["fileData"].(map[string]any); okFile {
					if renameMapKeyIfAbsent(fileObj, "mime_type", "mimeType") {
						changed = true
					}
					if renameMapKeyIfAbsent(fileObj, "file_uri", "fileUri") {
						changed = true
					}
				}
				if frObj, okFR := partMap["functionResponse"].(map[string]any); okFR {
					if frParts, okFRParts := frObj["parts"].([]any); okFRParts {
						for _, fp := range frParts {
							fpMap, okFP := fp.(map[string]any)
							if !okFP {
								continue
							}
							if renameMapKeyIfAbsent(fpMap, "inline_data", "inlineData") {
								changed = true
							}
							if renameMapKeyIfAbsent(fpMap, "file_data", "fileData") {
								changed = true
							}
							if nestedInline, okNI := fpMap["inlineData"].(map[string]any); okNI {
								if renameMapKeyIfAbsent(nestedInline, "mime_type", "mimeType") {
									changed = true
								}
							}
							if nestedFile, okNF := fpMap["fileData"].(map[string]any); okNF {
								if renameMapKeyIfAbsent(nestedFile, "mime_type", "mimeType") {
									changed = true
								}
								if renameMapKeyIfAbsent(nestedFile, "file_uri", "fileUri") {
									changed = true
								}
							}
						}
					}
				}
			}
		}
	}
	if !changed {
		return payload
	}
	out, err := marshalAntigravityJSONNoEscape(root)
	if err != nil {
		return payload
	}
	return out
}

func renameMapKeyIfAbsent(m map[string]any, snake, camel string) bool {
	val, ok := m[snake]
	if !ok {
		return false
	}
	if _, exists := m[camel]; !exists {
		m[camel] = val
	}
	delete(m, snake)
	return true
}

// WrapAntigravityAICodeSSELine wraps an unwrapped SSE line ("data: {...}")
// from aicode.googleapis.com into Cloud Code's "data: {\"response\":...}" shape
// so replay accumulators, usage filters, and response translators see one uniform format.
func WrapAntigravityAICodeSSELine(line []byte) []byte {
	payload := JSONPayload(line)
	if payload == nil {
		return line
	}
	wrapped := WrapAntigravityAICodeSSEPayload(payload)
	if bytes.Equal(wrapped, payload) {
		return line
	}
	out := make([]byte, 0, len(wrapped)+6)
	out = append(out, "data: "...)
	out = append(out, wrapped...)
	return out
}

// WrapAntigravityAICodeSSEPayload wraps an unwrapped aicode.googleapis.com
// GenerateContentResponse JSON chunk into Cloud Code's {"response": ...} shape
// so downstream SSE translators and non-stream reassembly handle both endpoints
// identically.
func WrapAntigravityAICodeSSEPayload(jsonPayload []byte) []byte {
	trimmed := bytes.TrimSpace(jsonPayload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return jsonPayload
	}
	if ojson.GetBytes(trimmed, "response").Exists() {
		return jsonPayload
	}
	if !ojson.GetBytes(trimmed, "candidates").Exists() && !ojson.GetBytes(trimmed, "usageMetadata").Exists() {
		return jsonPayload
	}
	out := make([]byte, 0, len(trimmed)+14)
	out = append(out, `{"response":`...)
	out = append(out, trimmed...)
	out = append(out, '}')
	return out
}

// IsAntigravityGoogleHost reports whether rawURL points at a real Google
// Cloud Code / AICode host (used to avoid firing companion calls against local
// httptest unit-test servers).
func IsAntigravityGoogleHost(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return host == "googleapis.com" || strings.HasSuffix(host, ".googleapis.com")
}

func marshalAntigravityJSONNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

func setAntigravityJSONStringNoEscape(payload []byte, path, value string) []byte {
	raw, err := marshalAntigravityJSONNoEscape(value)
	if err != nil {
		return payload
	}
	updated, errSet := ojson.SetRawBytes(payload, path, raw)
	if errSet != nil {
		return payload
	}
	return updated
}

func buildAntigravityStringReplacer(replacements map[string]string) *strings.Replacer {
	if len(replacements) == 0 {
		return nil
	}
	keys := make([]string, 0, len(replacements))
	for k := range replacements {
		if k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	pairs := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		pairs = append(pairs, k, replacements[k])
	}
	return strings.NewReplacer(pairs...)
}

// ApplyAntigravityToolDescriptionReplacements applies deterministic plain-text
// substring replacements (longest key first, no zero-width characters) to
// request.tools function declaration descriptions and nested JSON Schema
// description strings without modifying tool names, property keys, enums,
// defaults, or other schema keywords.
func ApplyAntigravityToolDescriptionReplacements(payload []byte, replacements map[string]string) []byte {
	if len(payload) == 0 || len(replacements) == 0 {
		return payload
	}
	toolsRes := util.GetGJSONBytesNoCopy(payload, "request.tools")
	if !toolsRes.Exists() || !toolsRes.IsArray() {
		return payload
	}
	replacer := buildAntigravityStringReplacer(replacements)
	if replacer == nil {
		return payload
	}

	dec := json.NewDecoder(strings.NewReader(toolsRes.Raw))
	dec.UseNumber()
	var tools []any
	if err := dec.Decode(&tools); err != nil || len(tools) == 0 {
		return payload
	}

	changed := false
	for _, toolItem := range tools {
		toolMap, okTool := toolItem.(map[string]any)
		if !okTool {
			continue
		}
		for _, declKey := range []string{"functionDeclarations", "function_declarations"} {
			decls, okDecls := toolMap[declKey].([]any)
			if !okDecls {
				continue
			}
			for _, declItem := range decls {
				declMap, okDecl := declItem.(map[string]any)
				if !okDecl {
					continue
				}
				for _, descKey := range []string{"description", "shortDescription", "short_description"} {
					if desc, okDesc := declMap[descKey].(string); okDesc {
						if updated := replacer.Replace(desc); updated != desc {
							declMap[descKey] = updated
							changed = true
						}
					}
				}
				for _, schemaKey := range []string{
					"parameters",
					"parametersJsonSchema",
					"parameters_json_schema",
					"response",
					"responseJsonSchema",
					"response_json_schema",
				} {
					if schemaMap, okSchema := declMap[schemaKey].(map[string]any); okSchema {
						if replaceAntigravitySchemaDescriptions(schemaMap, replacer) {
							changed = true
						}
					}
				}
			}
		}
	}
	if !changed {
		return payload
	}
	rawTools, errMarshal := marshalAntigravityJSONNoEscape(tools)
	if errMarshal != nil {
		return payload
	}
	updated, errSet := ojson.SetRawBytes(payload, "request.tools", rawTools)
	if errSet != nil {
		return payload
	}
	return updated
}

func replaceAntigravitySchemaDescriptions(schema map[string]any, replacer *strings.Replacer) bool {
	changed := false
	if desc, okDesc := schema["description"].(string); okDesc {
		if updated := replacer.Replace(desc); updated != desc {
			schema["description"] = updated
			changed = true
		}
	}
	for _, mapKey := range []string{"properties", "patternProperties", "$defs", "defs", "definitions", "dependentSchemas", "dependencies"} {
		childMap, okMap := schema[mapKey].(map[string]any)
		if !okMap {
			continue
		}
		for _, child := range childMap {
			if childSchema, okSchema := child.(map[string]any); okSchema {
				if replaceAntigravitySchemaDescriptions(childSchema, replacer) {
					changed = true
				}
			}
		}
	}
	for _, subKey := range []string{
		"items",
		"additionalItems",
		"additionalProperties",
		"additionalPropertiesSchema",
		"unevaluatedProperties",
		"unevaluatedItems",
		"contentSchema",
		"contains",
		"not",
		"if",
		"then",
		"else",
		"propertyNames",
	} {
		switch sub := schema[subKey].(type) {
		case map[string]any:
			if replaceAntigravitySchemaDescriptions(sub, replacer) {
				changed = true
			}
		case []any:
			for _, elem := range sub {
				if elemSchema, okSchema := elem.(map[string]any); okSchema {
					if replaceAntigravitySchemaDescriptions(elemSchema, replacer) {
						changed = true
					}
				}
			}
		}
	}
	for _, listKey := range []string{"prefixItems", "anyOf", "oneOf", "allOf"} {
		list, okList := schema[listKey].([]any)
		if !okList {
			continue
		}
		for _, elem := range list {
			if elemSchema, okSchema := elem.(map[string]any); okSchema {
				if replaceAntigravitySchemaDescriptions(elemSchema, replacer) {
					changed = true
				}
			}
		}
	}
	return changed
}

// NormalizeAntigravitySystemInstructionParts collapses multi-part text-only
// request.systemInstruction.parts (and snake_case request.system_instruction.parts)
// into a single text part joined by "\n\n", matching agy's single-part
// systemInstruction shape.
func NormalizeAntigravitySystemInstructionParts(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	for _, partsPath := range []string{"request.systemInstruction.parts", "request.system_instruction.parts"} {
		parts := ojson.GetBytes(payload, partsPath)
		if !parts.IsArray() {
			continue
		}
		arr := parts.Array()
		if len(arr) <= 1 {
			continue
		}
		texts := make([]string, 0, len(arr))
		textOnly := true
		for _, part := range arr {
			if !part.IsObject() {
				textOnly = false
				break
			}
			m := part.Map()
			if len(m) != 1 {
				textOnly = false
				break
			}
			txtVal, ok := m["text"]
			if !ok || txtVal.Type != ojson.String {
				textOnly = false
				break
			}
			if t := txtVal.String(); t != "" {
				texts = append(texts, t)
			}
		}
		if !textOnly {
			continue
		}
		joined := strings.Join(texts, "\n\n")
		singlePart, errMarshal := marshalAntigravityJSONNoEscape([]map[string]string{{"text": joined}})
		if errMarshal != nil {
			continue
		}
		if updated, errSet := ojson.SetRawBytes(payload, partsPath, singlePart); errSet == nil {
			payload = updated
		}
	}
	return payload
}

func antigravityContentHasFunctionResponse(content ojson.Result) bool {
	parts := content.Get("parts")
	if !parts.IsArray() {
		return false
	}
	found := false
	parts.ForEach(func(_, part ojson.Result) bool {
		if part.Get("functionResponse").Exists() || part.Get("function_response").Exists() {
			found = true
			return false
		}
		return true
	})
	return found
}

// WrapAntigravityInitialUserRequest wraps all human user turns with agy's
// <USER_REQUEST> and <ADDITIONAL_METADATA> tags using createdAt formatted in
// the current local timezone. For multi-turn conversations with distinct
// per-turn frozen timestamps, prefer WrapAntigravityUserRequests.
func WrapAntigravityInitialUserRequest(payload []byte, createdAt time.Time) []byte {
	return WrapAntigravityUserRequests(payload, "", createdAt)
}

// WrapAntigravityUserRequests wraps every human user turn's target text part
// (skipping functionResponse turns and already-wrapped turns) with agy 1.5.16's
// <USER_REQUEST> and <ADDITIONAL_METADATA> tags using each turn's frozen local
// timestamp so subsequent turns remain byte-identical for prompt caching.
func WrapAntigravityUserRequests(payload []byte, trajectoryID string, createdAt time.Time) []byte {
	if len(payload) == 0 {
		return payload
	}
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		return payload
	}
	type wrapTarget struct {
		contentIdx  int
		partIdx     int
		turnOrdinal int
		origText    string
	}
	var targets []wrapTarget
	userTurnOrdinal := 0
	contents.ForEach(func(ci, content ojson.Result) bool {
		if !strings.EqualFold(content.Get("role").String(), "user") {
			return true
		}
		if antigravityContentHasFunctionResponse(content) {
			return true
		}
		ordinal := userTurnOrdinal
		userTurnOrdinal++
		pi, txt := antigravityFirstUserTargetText(content.Get("parts"))
		if pi < 0 || txt == "" {
			return true
		}
		trimmedOrig := strings.TrimSpace(txt)
		if strings.HasPrefix(trimmedOrig, "<USER_REQUEST>") && strings.Contains(trimmedOrig, "</USER_REQUEST>") {
			return true
		}
		targets = append(targets, wrapTarget{
			contentIdx:  int(ci.Int()),
			partIdx:     pi,
			turnOrdinal: ordinal,
			origText:    txt,
		})
		return true
	})
	loc := antigravityNow().Location()
	for _, target := range targets {
		var ts time.Time
		if trajectoryID != "" {
			ts = ResolveAntigravityUserTurnTimestamp(trajectoryID, target.turnOrdinal, createdAt)
		} else if !createdAt.IsZero() {
			ts = createdAt
		} else {
			ts = antigravityNow()
		}
		wrapped := fmt.Sprintf("<USER_REQUEST>\n%s\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: %s.\n</ADDITIONAL_METADATA>", target.origText, ts.In(loc).Format(time.RFC3339))
		path := fmt.Sprintf("request.contents.%d.parts.%d.text", target.contentIdx, target.partIdx)
		payload = setAntigravityJSONStringNoEscape(payload, path, wrapped)
	}
	return payload
}

func isNativeAntigravityDecimalToolCallID(id string) bool {
	if !strings.HasPrefix(id, "call_") || len(id) <= len("call_") {
		return false
	}
	for i := len("call_"); i < len(id); i++ {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	return true
}

func deterministicAntigravityDecimalToolCallID(rawID string) string {
	sum := sha256.Sum256([]byte("antigravity-tool-id\x00" + rawID))
	return fmt.Sprintf("call_%d", binary.BigEndian.Uint64(sum[:8]))
}

func antigravityPartHasRealThoughtSignature(part ojson.Result) bool {
	for _, key := range []string{"thoughtSignature", "thought_signature", "extra_content.google.thought_signature"} {
		sig := strings.TrimSpace(part.Get(key).String())
		if sig != "" && !internalsignature.IsAntigravityThoughtSignatureBypass(sig) {
			return true
		}
	}
	return false
}

// NormalizeAntigravityToolCallIDs deterministically rewrites any non-decimal
// unsigned/bypass-signed functionCall.id and functionResponse.id values in
// request.contents (both camelCase and snake_case, such as toolu_... or hex
// call_abc...) to Gemini/agy's native call_<digits> format while keeping
// call/response pairs aligned across turns. Tool calls carrying a real upstream
// thoughtSignature (including parallel calls in the same signed model turn) are
// preserved verbatim so cryptographic signature validation and replay ledgers
// stay intact.
func NormalizeAntigravityToolCallIDs(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		return payload
	}
	var protectedIDs map[string]struct{}
	contents.ForEach(func(_, content ojson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}
		turnSigned := false
		parts.ForEach(func(_, part ojson.Result) bool {
			if antigravityPartHasRealThoughtSignature(part) {
				turnSigned = true
				return false
			}
			return true
		})
		if !turnSigned {
			return true
		}
		parts.ForEach(func(_, part ojson.Result) bool {
			for _, field := range []string{"functionCall", "function_call"} {
				idVal := part.Get(field + ".id")
				if idVal.Exists() && idVal.Type == ojson.String {
					if id := strings.TrimSpace(idVal.String()); id != "" {
						if protectedIDs == nil {
							protectedIDs = make(map[string]struct{})
						}
						protectedIDs[id] = struct{}{}
					}
				}
			}
			return true
		})
		return true
	})

	type idEdit struct {
		path  string
		newID string
	}
	var edits []idEdit
	contents.ForEach(func(ci, content ojson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}
		parts.ForEach(func(pi, part ojson.Result) bool {
			for _, field := range []string{"functionCall", "function_call", "functionResponse", "function_response"} {
				idVal := part.Get(field + ".id")
				if !idVal.Exists() || idVal.Type != ojson.String {
					continue
				}
				id := strings.TrimSpace(idVal.String())
				if id == "" || isNativeAntigravityDecimalToolCallID(id) {
					continue
				}
				if _, protected := protectedIDs[id]; protected {
					continue
				}
				edits = append(edits, idEdit{
					path:  fmt.Sprintf("request.contents.%d.parts.%d.%s.id", ci.Int(), pi.Int(), field),
					newID: deterministicAntigravityDecimalToolCallID(id),
				})
			}
			return true
		})
		return true
	})
	if len(edits) == 0 {
		return payload
	}
	out := payload
	for _, ed := range edits {
		if updated, errSet := ojson.SetBytes(out, ed.path, ed.newID); errSet == nil {
			out = updated
		}
	}
	return out
}
