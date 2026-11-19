# Configuration

Everything is set with environment variables (in Docker: `-e`, `env_file` or `environment:`). An invalid value stops the server at startup with a message naming the variable. Lists are comma-separated. Durations take days or Go units: `30d`, `12h`, `90m`.

[.env.example](../.env.example) has every variable with a comment; copy it to `.env` and fill in what you need.

## Server

| Variable | Default | |
|---|---|---|
| `APP_NAME` | `Exception Monitoring` | Shown in the UI, emails and notifications, and to coding agents, who register the MCP server under it in lowercase with dashes (`Acme Errors` → `acme-errors`). Up to 60 characters. |
| `PORT` | `8000` | Port to listen on. |
| `DATA_DIR` | `./data` (`/data` in the image) | Where the database (`leveldb`) lives, and `secret.key` when `SECRET_KEY` is not set. Keep it on a volume. |
| `PUBLIC_URL` | | The address users and SDKs reach the server at, e.g. `https://errors.example.com` (a path prefix is kept). Used for links in notifications, the DSNs on the Projects page and the MCP address. Without it, DSNs use the address the UI was opened with and notifications have no links. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. `debug` also logs every request and every rejected envelope. |
| `GOMEMLIMIT` | `200MiB` | The memory the server aims to stay under: the Go runtime collects garbage harder as it nears it. A soft limit, not a cap. Takes `MiB`, `GiB`. |
| `LOG_FORMAT` | `auto` | `auto`: readable colored lines in a terminal, logfmt otherwise (e.g. `docker logs`). Or `pretty`, `text`, `json`. `NO_COLOR` turns colors off. |

## Projects

| Variable | Default | |
|---|---|---|
| `PROJECTS` | | Optional: `id:name:public_key,...`, e.g. `1:backend:<key>,2:frontend:<key>`. The key is 16–64 letters or digits (`openssl rand -hex 16`). Written to the database at every start; such projects can only change their color and levels in the UI. Projects can also be added in the UI. |

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

Where messages go (email addresses, Telegram chats, Slack channels) is set up on the **Notifications** page of the UI. Telegram and Slack need nothing here: their tokens are entered in the form. Email needs a mail server.

| Variable | Default | |
|---|---|---|
| `SECRET_KEY` | generated | At least 32 characters (`openssl rand -hex 32`). Encrypts Telegram tokens and Slack webhooks and tokens in the database. Without it a key is generated at the first start into `DATA_DIR/secret.key`; that protects a leaked copy of the database (`DATA_DIR/leveldb`) but not a copy of the whole data directory, so on a server set it and keep it outside. Do not lose or change it: stored chats stop working (enter their tokens again). |
| `SMTP_HOST` | | Mail server; required for email. |
| `SMTP_PORT` | `587` | Port 465 uses TLS from the start, others STARTTLS. |
| `SMTP_USERNAME`, `SMTP_PASSWORD` | | SMTP login. |
| `EMAIL_FROM` | | Sender address; required with `SMTP_HOST`. |
| `NOTIFY_RETRY_FOR` | `1d` | A message that cannot be delivered is retried (2s, 10s, 30s, … then hourly) this long, then marked failed. |
| `NOTIFY_KEEP_SENT` | `1d` | Delivered messages are deleted from the delivery log after this. |
| `NOTIFY_KEEP_FAILED` | `30d` | Failed messages are deleted after this. |

Email addresses are confirmed with a code before they receive anything. Telegram and Slack connections can be tested from the form before saving.

## Retention

While the limit is on, events lose their details as they turn a month old, every 10 minutes (breadcrumbs, request, tags, contexts; the exception and its stack trace stay, and the event's page says details were removed). Nothing is deleted by age alone: data stays until the database nears its size limit.

| Variable | Default | |
|---|---|---|
| `MAX_DATABASE_SIZE` | `2GB` | Once the database reaches it, the least useful data is deleted until the database is at 80%. Takes `KB`, `MB`, `GB`; `0` turns the limit off, and nothing is deleted or thinned. |

The size is checked at startup, every 10 minutes and after every 50 MB written. The ingest queue is compacted at startup and every 10 minutes halfway between the checks, limit or not, so envelopes that waited there as the SDKs sent them, credentials included, leave the files; not after the writes of a peak, where it would add a quarter to the disk writes. Only the database files in use count (`DATA_DIR/leveldb`). At the limit the parts that collect deleted data as the server works (the ingest queue, issues) are first compacted, since deleted data keeps its space until then; if the database is still at the limit, the cleanup runs and always deletes:

1. Logs older than a month.
2. Events older than a month beyond the newest 20 of their issue.
3. The events of resolved and ignored issues but their latest.
4. Logs below error (trace, debug, info, warn), whatever their age.

Then, only while the database is still above 80%, each step after the one before:

5. Error and fatal logs together, oldest first.
6. Every issue down to its newest 20 events (new events are not capped as they arrive).
7. Events older than a week, oldest first whatever their level, but the latest of each issue.
8. Events of the last week, oldest first whatever their level, but the latest of each issue.
9. The latest event of issues not seen for a week, longest unseen first: the issue stays listed with its title, count and dates.
10. Whole issues, longest unseen first.

It deletes in batches of up to 2000 records and stops as soon as enough is freed. Each batch looks at the data as it is then, so an issue that gets a new event or is reopened meanwhile is judged by that. A cleanup stopped by a restart goes on where it stopped. Each run is logged with what every step deleted per project, and the last 100 runs with their steps are on the Projects page (Cleanup history).

A message logged over and over keeps its newest 10 copies as it is written (numbers and IDs in it do not count as a difference); the issue's event count always includes every event.
