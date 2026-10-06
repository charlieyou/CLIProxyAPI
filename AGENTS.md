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
- A 1 GB encrypted volume named `proxy_data` is mounted at `/data`. Runtime config is `/data/config.yaml`, with `auth-dir: /data/auths`. Credentials must persist on this volume, never in Git or a container image.
- Runtime security settings: `ws-auth: true`, debug logging off, remote management disabled, and control panel disabled. The endpoint uses the existing proxy API key; do not print it in logs or documentation.
- As of September 26, 2026, Fly holds four Claude accounts and two ChatGPT accounts. The local `~/.cli-proxy-api` directory was subsequently deleted; Fly now owns these credentials. Avoid concurrent refreshes of the same OAuth account on separate deployments.
- The initial deployment packaged the existing `/opt/cliproxyapi/cli-proxy-api` binary using `/home/cyou/.amp/in/cliproxyapi-fly/`. The repository's `fly.toml` instead builds this checkout using its Dockerfile and explicitly starts `/CLIProxyAPI/CLIProxyAPI --config /data/config.yaml`. Redeploying from this repository therefore upgrades to the checked-out source, rather than reusing the initial binary.
- The current image also includes `/usr/local/bin/aim`, built from `/home/cyou/aim`. Local `~/go/bin/aim` invokes it over Fly SSH with `--auth-dir /data/auths --read-only`; CLIProxyAPI alone owns refresh. Rebuild AIM into the deployment context with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /home/cyou/.amp/in/cliproxyapi-fly/aim .` from the AIM repo, then deploy that context. The repository Dockerfile does not include AIM: preserve this additional binary when switching to a repository-source deployment, or the local AIM wrapper will stop working.
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
- Follow `gofmt`; keep imports goimports-style; wrap errors with context where helpful
- Do not use `log.Fatal`/`log.Fatalf` (terminates the process); prefer returning errors and logging via logrus
- Shadowed variables: use method suffix (`errStart := server.Start()`)
- Wrap defer errors: `defer func() { if err := f.Close(); err != nil { log.Errorf(...) } }()`
- Use logrus structured logging; avoid leaking secrets/tokens in logs
- Avoid panics in HTTP handlers; prefer logged errors and meaningful HTTP status codes
- Timeouts are allowed only during credential acquisition; after an upstream connection is established, do not set timeouts for any subsequent network behavior. Intentional exceptions that must remain allowed are the Codex websocket liveness deadlines in `internal/runtime/executor/codex_websockets_executor.go`, the wsrelay session deadlines in `internal/wsrelay/session.go`, the management APICall timeout in `internal/api/handlers/management/api_tools.go`, and the `cmd/fetch_antigravity_models` utility timeouts
- Avoid wall-clock `time.Sleep` in TTL, expiration, ordering, or cache-eviction unit tests due to platform timer granularity (e.g. Windows default timer resolution of ~15.6ms) and CI jitter under load; prefer controllable clocks (`nowFunc` / mock clock), explicit timestamp manipulation, or deterministic synchronization primitives.
- Note: if modifying features that involve CLIProxyAPIHome, check if corresponding updates are needed in the CLIProxyAPIHome repository.
