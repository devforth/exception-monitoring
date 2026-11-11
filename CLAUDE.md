# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Lightweight self-hosted error and log monitoring server that accepts data from the official Sentry SDKs via DSN (envelope protocol). Go, embedded SQLite (`modernc.org/sqlite`, no CGO), shipped mainly as a Docker image (`FROM scratch`, size budget 50 MB, currently ~16 MB). MIT, public DevForth OSS project. Tracklify task: `DevFo-OSS/17`.

Scope: errors and logs only. Tracing spans, session replay, uptime and `client_report` items are accepted and dropped, not stored.

Planned stages: 0 protocol research (done), 1 skeleton (done), 2 ingest, 3 data model and grouping, 4 REST API, 5 UI (htmx), 6 notifications (Telegram first, then email in full format, Slack short), 7 retention, 8 packaging and docs, 9 MCP. Design the REST API so an MCP server can be layered on later (GlitchTip's MCP tool set is the reference).

## Commands

Go is installed in `~/.local/go`; if `go` is not on PATH, run `export PATH="$HOME/.local/go/bin:$HOME/go/bin:$PATH"` first.

```sh
make test         # go test -race ./...
make lint         # go vet + gofmt check (CI fails on unformatted files)
make build        # static binary to bin/exception-monitoring
make docker       # build image devforth/exception-monitoring:dev
make image-size   # fails if the image is over MAX_IMAGE_MB (50)

go test ./internal/store -run TestNewerSchemaRejected -count=1   # single test
```

Do not start the server (`make run`, `docker run`, …) yourself; the user runs it in their own terminal. Check it with `curl localhost:8000/health` and ask the user to start it if it is not reachable.

CI (`.github/workflows/ci.yml`) runs `make lint`, `make test`, an amd64 image build with the size check and an arm64 build.

## Architecture

- `cmd/exception-monitoring/main.go`: loads config, opens the store, binds the listener before logging "server started", graceful shutdown on SIGTERM. The `healthcheck` subcommand is what Docker's HEALTHCHECK calls (no curl in a scratch image).
- `internal/config`: everything comes from env (`PORT`, `DATA_DIR`, `LOG_LEVEL`, `PROJECTS`, `ALLOWED_ORIGINS`, `RATE_LIMIT_PER_MINUTE`). `Load()` wraps `loadFrom(getenv)` so tests inject a map instead of touching the process env.
- `internal/store`: SQLite with WAL, `busy_timeout`, `foreign_keys`, and `_txlock=immediate`, which prevents "database is locked" on concurrent read-then-write transactions (there is a test for it). Migrations live in the `migrations` slice in `migrate.go`: versions must be sequential from 1, each runs in its own transaction, and never edit an applied migration (add a new one). Startup refuses a database whose schema is newer than the binary.
- `internal/server`: `net/http` ServeMux with method patterns (`"GET /health"`), wrapped in a request-logging middleware. The server depends on small interfaces (`Pinger`, `http.Handler` for ingest), so tests use fakes instead of a real database.
- `internal/sentry`: the SDK wire format only (envelope splitting, `Event`, `Log`), no HTTP or storage. Decoding is deliberately lenient: custom types (`Timestamp`, `Message`, `Tags`, `ExceptionList`, …) accept every shape the protocol allows and fall back to the zero value on an unexpected one, because losing a field is better than losing the error report. `Event.Raw` keeps the full payload.
- `internal/ingest`: `POST /api/{projectID}/envelope/`. Order matters: CORS → public key (query `sentry_key`, `X-Sentry-Auth`/`Authorization`, or the envelope header `dsn` when sent via a tunnel) → project auth → per-project rate limit → read/decompress body (gzip, deflate; 20 MB cap before and after decompression) → parse all items → `Sink`. All items are parsed before anything is stored, so a malformed item rejects the whole envelope. Only `event` and `log` items reach the `Sink`; other types are dropped. `LogSink` is a temporary sink that only logs; it gets replaced by a store-backed sink in stage 3. Rejections are logged at debug level (ingest is public, so warn-level logging would be a flood vector).

### Projects and DSNs

`PROJECTS=1:backend:<key>,2:frontend:<key>` — `id:name:public_key`. IDs are chosen by the user and must stay stable: the id is part of the DSN (`http://<key>@host:8000/<id>`) and will be stored with every event, so it must never be derived from list order. Each service (backend, SSR, frontend…) gets its own project; all live in one container and the UI shows which project an error came from.

### Protocol reference material

- `docs/sentry-protocol.md`: notes on the envelope format as actually sent by Sentry SDK 11 (Node and browser): auth via `sentry_key` query or `X-Sentry-Auth` header, missing/`text/plain` Content-Type, item types, log batches, cause chains (innermost first), `fingerprint`, `trace_id`.
- `testdata/envelopes/*.envelope` + `*.request.json`: real, sanitized SDK captures. `internal/sentry` tests parse every fixture; `internal/ingest` tests replay every recorded request through the handler. A new fixture is picked up automatically.
- `tools/envelope-dumper/dumper.mjs`: Node research server that writes incoming envelopes to `envelopes/` (gitignored). Used together with the separate `../sentry-playground` AdminForth app, which is the test client (routes under `/api/debug-sentry/`).

Planned auth model: one admin from env plus an IP allowlist for the UI and API; the ingest endpoint stays open (protected only by DSN keys, CORS `ALLOWED_ORIGINS` and rate limiting) because browsers send errors directly.

## Conventions

- Tests sit next to the code (`*_test.go`, same package); fixtures go in `testdata/`.
- Use descriptive variable names (`settings`, `database`, `transaction`, `migrationList`), not one-letter ones. Keep the standard Go idioms `err`, `ctx`, `t`, and `w`/`r` in HTTP handlers.
- Do not copy code from Bugsink (PolyForm Shield license) or Sentry (FSL). GlitchTip (MIT) is fine as a reference.
- Commit messages: one subject line containing `DevFo-OSS/17`, no body, no AI attribution. Commit only when asked.
- Keep the README abstract (goals and features). Implementation details are added only once the corresponding stage lands.
