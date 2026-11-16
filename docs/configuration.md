# Configuration

Everything is set with environment variables (in Docker: `-e`, `env_file` or `environment:`). An invalid value stops the server at startup with a message naming the variable. Lists are comma-separated. Durations take days or Go units: `30d`, `12h`, `90m`.

[.env.example](../.env.example) has every variable with a comment; copy it to `.env` and fill in what you need.

## Server

| Variable | Default | |
|---|---|---|
| `APP_NAME` | `Exception Monitoring` | Shown in the UI, emails and notifications, and to coding agents, who register the MCP server under it in lowercase with dashes (`Acme Errors` → `acme-errors`). Up to 60 characters. |
| `PORT` | `8000` | Port to listen on. |
| `DATA_DIR` | `./data` (`/data` in the image) | Where the SQLite database (`database.db`) lives. Keep it on a volume. |
| `PUBLIC_URL` | | The address users and SDKs reach the server at, e.g. `https://errors.example.com` (a path prefix is kept). Used for links in notifications, the DSNs on the Projects page and the MCP address. Without it, DSNs use the address the UI was opened with and notifications have no links. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. `debug` also logs every request and every rejected envelope. |
| `LOG_FORMAT` | `auto` | `auto`: readable colored lines in a terminal, logfmt otherwise (e.g. `docker logs`). Or `pretty`, `text`, `json`. `NO_COLOR` turns colors off. |

## Projects

| Variable | Default | |
|---|---|---|
| `PROJECTS` | | Optional: `id:name:public_key,...`, e.g. `1:backend:<key>,2:frontend:<key>`. The key is 16–64 letters or digits (`openssl rand -hex 16`). Written to the database at every start; such projects can only change their retention and levels in the UI. Projects can also be added in the UI. |

The id is part of the DSN (`https://<public_key>@errors.example.com/<id>`) and of every stored event, so never change or reuse it. Name each service (backend, frontend, …) as its own project.

## Access

| Variable | Default | |
|---|---|---|
| `ADMIN_USERNAME` | `admin` | The superadmin, created at the first start only. |
| `ADMIN_PASSWORD` | | At least 12 characters. Needed for the first start; later changes are ignored (change the password in the UI, or see [forgotten password](deployment.md#forgotten-password)). Until it is set, nobody can log in. |
| `SESSION_LIFETIME` | `1d` | How long a login to the UI lasts (`12h`, `7d`); it is not extended by use, so then you log in again. |
| `API_TOKEN` | | Token of the [REST API](api.md), at least 32 characters (`openssl rand -hex 32`). Without it the REST API is closed. MCP does not use it. |
| `ALLOWED_IPS` | any | Addresses or networks (`203.0.113.7,10.0.0.0/8`) allowed to open the UI, REST API and MCP. Ingest and `/health` stay open. |
| `TRUSTED_PROXIES` | | Reverse proxies (addresses or networks) whose `X-Forwarded-For` and `X-Forwarded-Proto` are believed. Needed behind a proxy: for the client address (`ALLOWED_IPS`, login limits) and to know the page is HTTPS (secure cookies, DSNs). |

## Ingest

| Variable | Default | |
|---|---|---|
| `ALLOWED_ORIGINS` | `*` | Browser origins allowed to send events (CORS), e.g. `https://app.example.com,https://admin.example.com`. Backend SDKs send no origin and are always allowed. |
| `RATE_LIMIT_PER_MINUTE` | `1000` | Envelopes per project per minute; over it SDKs get 429 and back off. `0` = no limit. |
| `RECORD_EVENT_LEVELS` | `error,fatal` | Event levels to store: `fatal`, `error`, `warning`, `info`, `debug`, or `all`/`none`. Others are dropped on arrival and never notify. A project can choose its own on the Projects page. |
| `RECORD_LOG_LEVELS` | `all` | Log levels to store: `fatal`, `error`, `warn`, `info`, `debug`, `trace`, or `all`/`none`. |

## Notifications

Where messages go (email addresses, Telegram chats, Slack channels) is set up on the **Notifications** page of the UI. The variables only enable the types.

| Variable | Default | |
|---|---|---|
| `NOTIFY_CHANNEL_TYPES` | `email,telegram,slack` | Types that can be added; `none` for no notifications. |
| `SECRET_KEY` | | At least 32 characters (`openssl rand -hex 32`). Encrypts Telegram tokens and Slack webhooks and tokens in the database; required for these two types. Keep it outside the data directory and do not lose it: stored chats stop working without it. |
| `SMTP_HOST` | | Mail server; required for email. |
| `SMTP_PORT` | `587` | Port 465 uses TLS from the start, others STARTTLS. |
| `SMTP_USERNAME`, `SMTP_PASSWORD` | | SMTP login. |
| `EMAIL_FROM` | | Sender address; required with `SMTP_HOST`. |
| `NOTIFY_RETRY_FOR` | `1d` | A message that cannot be delivered is retried (2s, 10s, 30s, … then hourly) this long, then marked failed. |
| `NOTIFY_KEEP_SENT` | `1d` | Delivered messages are deleted from the delivery log after this. |
| `NOTIFY_KEEP_FAILED` | `30d` | Failed messages are deleted after this. |

Email addresses are confirmed with a code before they receive anything. Telegram and Slack connections can be tested from the form before saving.

## Retention

Old data is deleted at startup and every 10 minutes. `0` turns a limit off. Every limit except the size can be overridden per project on the Projects page.

| Variable | Default | |
|---|---|---|
| `RETENTION_EVENTS` | `30d` | Delete events received longer ago. |
| `RETENTION_LOGS` | `14d` | Delete older logs. |
| `RETENTION_EVENTS_PER_ISSUE` | `100` | Keep only the newest events of each issue; the issue's count still includes all. |
| `RETENTION_LOGS_PER_MESSAGE` | `50` | Keep the newest logs of a message logged over and over (numbers and IDs in it do not count as a difference). |
| `RETENTION_ISSUES` | `365d` | Delete issues, with their events, not seen this long. |
| `MAX_DATABASE_SIZE` | `2GB` | Above it, delete the oldest events and logs down to 90%. Takes `KB`, `MB`, `GB`. |

Every issue keeps its latest event until the issue itself expires, so each error stays listed with its count and last stack trace.
