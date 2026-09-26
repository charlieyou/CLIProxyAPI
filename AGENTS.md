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
- Antigravity uses upstream's direct OAuth implementation against Google's Cloud Code API (Antigravity Hub fingerprint, version from the Hub updater manifest). The fork's agy CLI replication was dropped in the November 27, 2026 rebase; both use the same OAuth client, so existing credentials keep refreshing. Sign in with `fly ssh console -a cyou-cliproxyapi --pty -C '/CLIProxyAPI/CLIProxyAPI --config /data/config.yaml --antigravity-login --no-browser'`: open the printed link; the browser then fails to reach the `http://localhost:51121/oauth-callback` redirect, and about 15 seconds after the link the CLI prompts for the full address-bar URL (a bare code ends the login with "invalid callback URL"; the whole wait is 5 minutes). Sign-in gives new Antigravity credentials `"prefix": "google"`, as Claude accounts get `anthropic`, because Amp's wildcard mapping sends models as `google/<model>`; uploaded or sftp'd Antigravity files need the prefix added by hand. `/data/config.yaml` sets `oauth-excluded-models: {antigravity: ["claude-*", "gpt-*"]}`, so Antigravity serves only Gemini on Fly. Upstream registers one representative ID per Gemini thinking group (such as `gemini-4-neon-xhigh`) and picks the variant from the request's reasoning level; bare IDs such as `gemini-4-neon` need `oauth-model-alias` entries. `antigravity.tool-description-replacements` rewrites tool descriptions. The fork's `antigravity.system-prompt-replacements` key no longer exists and is silently ignored; move its entries to upstream's `sensitive-words-replace`. `antigravity.stop-on-bare-429` defaults to `false` (the fork's agy build defaulted to `true`); set it to `true` in `/data/config.yaml` to return bare 429s to the client instead of cooling the credential and rotating. `antigravity.max-concurrent-requests` and `antigravity.max-rpm` cap each credential's in-flight requests and requests per 60 seconds (0 disables). The Amp "Google CLIProxyAPI" connection is a Custom URL connection in `google-genai` format with base URL `https://cyou-cliproxyapi.fly.dev/v1beta` and mapping `google-vertex/gemini-*`.
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
