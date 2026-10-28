# Maildrop Agent Guide

## Commands
- **Build**: `go build -o maildrop ./cmd/maildrop`
- **Run**: `go run ./cmd/maildrop` (Starts Web + SMTP server + cleanup in one process; reads `.env` from the working directory)
- **Docker**: `docker compose up -d`
- **Test (Go)**: `go vet ./... && go test ./...`
- **Test (frontend contracts)**: `pip install -r requirements-dev.txt && python -m pytest -q tests`
- **Browser QA**: `python scripts/verify_ui_browser.py --quick --skip-perf` (builds and starts the Go binary on temp ports)

## Architecture
- **Stack**: Go (`net/http` + `emersion/go-smtp`) + SQLite (`modernc.org/sqlite`, pure Go, no CGO).
- **Entry**: `cmd/maildrop/main.go` wires config, store, HTTP, SMTP and cleanup, with graceful shutdown on SIGINT/SIGTERM.
- **Packages** (`internal/`):
  - `config`: env/.env loading (same variable names as the former Python version).
  - `store`: SQLite schema, migrations (compatible with Python-era `mailbox.db`), mailbox/email/domain/sub-admin persistence.
  - `inbox`: per-mailbox operations and SMTP delivery (routes by envelope recipients).
  - `admin`: admin mailbox management, stats, audit log, domain management.
  - `smtpd`: SMTP receiving. `mailparse`: MIME parsing. `ipguard`: IP blocker, whitelist, client IP.
  - `cleanup`: periodic expiry cleanup. `web`: pages, `/static`, `/api/*`, `/api/admin/*`.
- **Frontend**: `src/frontend` (plain HTML/JS, no build step), embedded into the binary via `embed.go`.
- **Data**: persisted in `data/mailbox.db`. JSON storage mode is no longer supported.
- **Domains**: managed in the admin UI (`域名设置`, `/api/admin/domains`); `DOMAINS` in `.env` only seeds an empty domain table on first start.
- **Design notes**: compatibility guarantees, intentional behavior changes vs. the former Python version and known issues are in `docs/go-rewrite.md`.

## Style & Conventions
- **Go**: gofmt; standard library first; pass dependencies explicitly (no global singletons); log via `log/slog`; short Chinese comments.
- **SQLite**: the store uses a single connection. Inside `store.WithTx` callbacks use only the passed `*sql.Tx`; never call `Store` methods or `Store.DB()` there (deadlock).
- **Schema**: migrations must keep opening Python-era databases. `internal/store/testdata/python_schema_*.json` is the compatibility baseline; do not edit it by hand.
- **Config**: specific configs in `.env`, loaded by `internal/config`.
- **API**: User routes at `/api/*`, Admin at `/api/admin/*`. Keep response field names and types stable; the frontend reads them directly.
- **Frontend**: Plain HTML/JS (no build step, no template syntax). Static paths are literal `/static/...`. Fetch API for backend interaction.
- **Safety**: Do not commit `.env` or data files.
