# AGENTS.md

Go 1.26+ proxy server providing OpenAI/Gemini/Claude/Codex compatible APIs with OAuth and round-robin load balancing.

## Repository
- GitHub: https://github.com/router-for-me/CLIProxyAPI

## Commands
```bash
gofmt -w . # Format (required after Go changes)
go build -o cli-proxy-api ./cmd/server # Build
go run ./cmd/server # Run dev server
go test ./... # Run all tests
go test -v -run TestName ./path/to/pkg # Run single test
go build -o test-output ./cmd/server && rm test-output # Verify compile (REQUIRED after changes)
```
- Common flags: `--config <path>`, `--tui`, `--standalone`, `--local-model`, `--no-browser`, `--oauth-callback-port <port>`

## Config
- Default config: `config.yaml` (template: `config.example.yaml`)
- `.env` is auto-loaded from the working directory
- Auth material defaults under `auths/`
- Storage backends: file-based default; optional Postgres/git/object store (`PGSTORE_*`, `GITSTORE_*`, `OBJECTSTORE_*`)

## Personal Fly.io deployment
- App: `cyou-cliproxyapi`, organization: `personal`, primary region: `iad` (Ashburn, Virginia).
- Public endpoint: `https://cyou-cliproxyapi.fly.dev`; OpenAI-compatible base URL: `https://cyou-cliproxyapi.fly.dev/v1`. Anthropic clients use the root URL, without `/v1`.
- One always-on shared-cpu-2x Machine with 1 GB RAM and 512 MB swap. Fly terminates HTTPS and forwards to port 8317; proxy idle timeout is 600 seconds, not a total request-duration setting.
- A 20 GB encrypted volume named `proxy_data` is mounted at `/data`. Runtime config is `/data/config.yaml`, with `auth-dir: /data/auths`. Credentials must persist on this volume, never in Git or a container image.
- Runtime security settings: `ws-auth: true`, debug logging off, remote management disabled, and control panel disabled. The endpoint uses the existing proxy API key; do not print it in logs or documentation.
- As of September 26, 2026, Fly holds four Claude accounts and two ChatGPT accounts. The local `~/.cli-proxy-api` directory was subsequently deleted; Fly now owns these credentials. Avoid concurrent refreshes of the same OAuth account on separate deployments.
- The initial deployment packaged the existing `/opt/cliproxyapi/cli-proxy-api` binary using `/home/cyou/.amp/in/cliproxyapi-fly/`. The repository's `fly.toml` instead builds this checkout using its Dockerfile and explicitly starts `/CLIProxyAPI/CLIProxyAPI --config /data/config.yaml`. Redeploying from this repository therefore upgrades to the checked-out source, rather than reusing the initial binary.
- Since November 21, 2026 the image no longer includes AIM (`/usr/local/bin/aim`); build it only from the repository Dockerfile, without extra binaries, so the local `~/go/bin/aim` Fly SSH wrapper no longer works.
- Antigravity requests run directly via OAuth against Google's Cloud Code API (`https://daily-cloudcode-pa.googleapis.com/v1internal:*`) replicating the official Antigravity CLI (`agy 1.5.16`) wire profile; the repository Dockerfile does not bundle `agy`. Upstream's Antigravity Hub implementation was replaced by this agy replication again after the November 27, 2026 rebase; both use the same OAuth client, so existing credentials keep refreshing. Sign in with `fly ssh console -a cyou-cliproxyapi --pty -C '/CLIProxyAPI/CLIProxyAPI --config /data/config.yaml --antigravity-login --no-browser'`: the CLI prints a Google PKCE OAuth link redirecting to `https://antigravity.google/oauth-callback`, reads back the authorization code (or full redirect URL), exchanges it with `code_verifier` and all 7 `agy` scopes (including `https://www.googleapis.com/auth/aicode` and `openid`), discovers `project_id` via `daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist`, runs `agy`'s post-login warmup sequence, and saves a direct OAuth credential with `"prefix": "google"` (its access token is refreshed lazily on demand when a request needs it, with no background refresh); uploaded or sftp'd Antigravity files need the prefix added by hand. All Cloud Code requests use HTTP/1.1 with TLS ALPN `["http/1.1"]` and `User-Agent: antigravity/cli/1.5.16 (aidev_client; os_type=<os>; arch=<arch>; cl=1028571191; auth_method=consumer; datacollection_opt_in=false)` (the version is pinned; the updater only logs it); streaming requests use `Transfer-Encoding: chunked`, 5-segment `agent/<cascade>/<ms>/<trajectory>/<step>` `requestId`, 64-bit FNV-1a `request.sessionId`, `request.labels` (with `model_enum`), and `agy`'s `thinkingConfig` wire format (`LOW`/`MEDIUM`/`HIGH` and `100` for `xhigh`). Live models are fetched from `v1internal:fetchAvailableModels` on startup and hourly, registering the full 1M context window (`1,048,576`) and synthesizing bare base IDs (such as `gemini-4-neon`, `gemini-4-carbon`, `gemini-3.8-flash`) that resolve to the matching effort-suffixed upstream model ID from the request's thinking config or suffix (defaulting to `high`), so bare IDs need no `oauth-model-alias` entries. `/data/config.yaml` sets `oauth-excluded-models: {antigravity: ["claude-*", "gpt-*"]}`, so Antigravity serves only Gemini on Fly. Antigravity options live under `oauth.providers.antigravity` (legacy top-level `antigravity:` still maps there): `system-prompt-replacements` and `tool-description-replacements` rewrite system-instruction and tool-description text, `move-system-instruction-to-user` (with optional `system-instruction-baseline`) moves the caller's system instruction into the first user turn, `sensitive-words` inserts U+200B, and upstream's `sensitive-words-replace`, `sensitive-patterns-replace` and `tos-pause-window` are not applied to Antigravity (use `system-prompt-replacements`). `stop-on-bare-429` defaults to `true`: a bare 429 is returned to the client without cooling the credential or rotating; set it to `false` to cool and rotate instead. `max-concurrent-requests` and `max-rpm` cap each credential's in-flight requests and requests per 60 seconds (0 disables). An Antigravity HTTP 403 (such as `VALIDATION_REQUIRED`) quarantines the credential (`"disabled": true, "quarantined": true`) until it is re-enabled or signed in again. The Amp "Google CLIProxyAPI" connection is a Custom URL connection in `google-genai` format with base URL `https://cyou-cliproxyapi.fly.dev/v1beta` and mapping `google-vertex/gemini-*`. Credentials from the earlier `agy acp` flow (holding only `agy_profile`) must be replaced with a fresh `--antigravity-login`.
- After explicit deployment authorization, deploy from this directory with `fly deploy --remote-only --ha=false`. Keep one Machine unless credential storage/replication is redesigned; Fly volumes are not automatically replicated. A single Machine has deployment and host-failure downtime; maintain credential backups.
- Inspect with `fly status -a cyou-cliproxyapi` and `fly ssh console -a cyou-cliproxyapi`. Upload individual credentials with `fly ssh sftp put <local-file> /data/auths/<filename> -a cyou-cliproxyapi -m 0600`; preserve existing credentials unless removal is requested.
- Claude login on the initial image: `fly ssh console -a cyou-cliproxyapi --pty -C '/CLIProxyAPI/cli-proxy-api --config /data/config.yaml --claude-login --no-browser'`. After a repository-source deployment, use `/CLIProxyAPI/CLIProxyAPI` instead. Use `--codex-login` for ChatGPT.
- Verify authenticated `/v1/models`, unauthenticated rejection, and an actual streaming inference for each provider. Model listing alone does not prove OAuth credentials work. Both Claude and ChatGPT streaming inference passed on September 26, 2026; the four subsequently copied credentials were not individually inference-tested.

## Architecture
- `cmd/server/` — Server entrypoint
- `internal/api/` — Gin HTTP API (routes, middleware, modules)
- `internal/api/modules/amp/` — Amp integration (Amp-style routes + reverse proxy)
- `internal/thinking/` — Main thinking/reasoning pipeline. `ApplyThinking()` (apply.go) parses suffixes (`suffix.go`, suffix overrides body), normalizes config to canonical `ThinkingConfig` (`types.go`), normalizes and validates centrally (`validate.go`/`convert.go`), then applies provider-specific output via `ProviderApplier`. Do not break this "canonical representation → per-provider translation" architecture.
- `internal/runtime/executor/` — Per-provider runtime executors (incl. Codex WebSocket)
- `internal/translator/` — Provider protocol translators (and shared `common`)
- `internal/registry/` — Model registry + remote updater (`StartModelsUpdater`); `--local-model` disables remote updates
- `internal/store/` — Storage implementations and secret resolution
- `internal/managementasset/` — Config snapshots and management assets
- `internal/cache/` — Request signature caching
- `internal/watcher/` — Config hot-reload and watchers
- `internal/wsrelay/` — WebSocket relay sessions
- `internal/usage/` — Usage and token accounting
- `internal/home/` — CLIProxyAPIHome control plane integration (bootstrap, RESP communication, dispatch coordination)
- `internal/tui/` — Bubbletea terminal UI (`--tui`, `--standalone`)
- `sdk/cliproxy/` — Embeddable SDK entry (service/builder/watchers/pipeline)
- `test/` — Cross-module integration tests

## Code Conventions
- Keep changes small and simple (KISS)
- Comments in English only
- If editing code that already contains non-English comments, translate them to English (don’t add new non-English comments)
- For user-visible strings, keep the existing language used in that file/area
- New Markdown docs should be in English unless the file is explicitly language-specific (e.g. `README_CN.md`)
- As a rule, do not make standalone changes to `internal/translator/`. You may modify it only as part of broader changes elsewhere.
- If a task requires changing only `internal/translator/`, run `gh repo view --json viewerPermission -q .viewerPermission` to confirm you have `WRITE`, `MAINTAIN`, or `ADMIN`. If you do, you may proceed; otherwise, file a GitHub issue including the goal, rationale, and the intended implementation code, then stop further work.
- `internal/runtime/executor/` should contain executors and their unit tests only. Place any helper/supporting files under `internal/runtime/executor/helps/`.
- Do not use `github.com/tidwall/gjson`, `github.com/tidwall/sjson`, Go's standard `encoding/json`, Go's standard `encoding/base64`, or Go's standard `regexp`. Always use `github.com/MoeForge/moego/json`, `github.com/MoeForge/moego/ojson`, `github.com/MoeForge/moego/base64`, and `github.com/MoeForge/moego/regexp` for all JSON, Base64, and regular expression operations. All JSON write operations (`Set`, `SetRaw`, `Delete`, etc.) MUST be performed on `*ojson.Doc` (`ojson.NewDoc` / `ojson.NewDocString`), passing `*ojson.Doc` across mutation stages and serializing only once at the final exit (`doc.Bytes()` / `doc.String()`), rather than mutating raw `[]byte` or `string` payloads directly.
- Payload configuration MUST be the final semantic barrier before sending requests in every executor, including streaming, WebSocket, continuation, retry/fallback, image, and token-count paths where applicable. Complete all built-in payload translation, normalization, injection, and cleanup first, then evaluate and apply user payload rules exactly once to the final business payload. No subsequent logic may overwrite configured values or restore filtered fields. Only necessary transport framing, serialization, signature calculation, and read-only validation may follow without changing business payload semantics. Add regression tests when introducing or changing request-building paths to enforce this ordering.
- Follow `gofmt`; keep imports goimports-style; wrap errors with context where helpful
- Do not use `log.Fatal`/`log.Fatalf` (terminates the process); prefer returning errors and logging via logrus
- Shadowed variables: use method suffix (`errStart := server.Start()`)
- Wrap defer errors: `defer func() { if err := f.Close(); err != nil { log.Errorf(...) } }()`
- Use logrus structured logging; avoid leaking secrets/tokens in logs
- Avoid panics in HTTP handlers; prefer logged errors and meaningful HTTP status codes
- Timeouts are allowed only during credential acquisition; after an upstream connection is established, do not set timeouts for any subsequent network behavior. Intentional exceptions that must remain allowed are the Codex websocket liveness deadlines in `internal/runtime/executor/codex_websockets_executor.go`, the wsrelay session deadlines in `internal/wsrelay/session.go`, the management APICall timeout in `internal/api/handlers/management/api_tools.go`, and the `cmd/fetch_antigravity_models` utility timeouts
- Avoid wall-clock `time.Sleep` in TTL, expiration, ordering, or cache-eviction unit tests due to platform timer granularity (e.g. Windows default timer resolution of ~15.6ms) and CI jitter under load; prefer controllable clocks (`nowFunc` / mock clock), explicit timestamp manipulation, or deterministic synchronization primitives.
- Note: if modifying features that involve CLIProxyAPIHome, check if corresponding updates are needed in the CLIProxyAPIHome repository.
- Endpoints under the `/v0/management` base URL are deprecated and no longer maintained. For any feature changes, do not modify endpoints under `/v0/management` unless necessary to fix compilation errors.
