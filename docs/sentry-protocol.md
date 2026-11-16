# Sentry ingest protocol: research notes

Observed by pointing real Sentry SDKs at `tools/envelope-dumper`. Captured with:

- `@sentry/node` 11.11.0 (Express app)
- `@sentry/browser` 11.11.0 (CDN bundle `bundle.tracing.logs.metrics.min.js`)

Sanitized samples are in [`testdata/envelopes/`](../testdata/envelopes). Each sample is a `<name>.envelope` body plus a `<name>.request.json` file with the method, path, query and relevant headers.

Not yet verified: Python and other SDKs, gzip-compressed bodies, the legacy `/store/` endpoint, and attachments.

## DSN

```
{scheme}://{public_key}@{host}[:port]/{project_id}
```

The SDK derives the ingest URL from the DSN:

```
POST {scheme}://{host}[:port]/api/{project_id}/envelope/
```

## Request

| Aspect | Node SDK | Browser SDK |
|---|---|---|
| Method | `POST` | `POST` |
| Auth | query: `sentry_key`, `sentry_version=7`, `sentry_client` | same, in query |
| `X-Sentry-Auth` header | not sent | not sent |
| `Content-Type` | **not sent** | `text/plain;charset=UTF-8` (avoids CORS preflight) |
| `Content-Encoding` | none observed (bodies up to 30 KB) | none |
| CORS | n/a | yes. The server must answer `OPTIONS` and send `Access-Control-Allow-Origin`. |

Server requirements:

- Accept the key from the query string **or** from the `X-Sentry-Auth: Sentry sentry_key=...` header, because other SDKs use the header.
- Ignore `Content-Type`.
- Support `Content-Encoding: gzip` (and ideally `deflate`/`br`).
- Respond `200` with `{"id": "<event_id>"}`. Use `429` + `Retry-After` / `X-Sentry-Rate-Limits` for rate limiting.

## Envelope format

Newline-delimited:

```
<envelope header JSON>\n
<item header JSON>\n
<item payload>\n
<item header JSON>\n
<item payload>\n
...
```

- If the item header has `length`, the payload is exactly that many bytes, which can be binary. Otherwise the payload runs to the next `\n`.
- **Envelope header:** `event_id` (events only), `sent_at`, `sdk {name, version}`, and `trace`. `trace` is the dynamic sampling context: `trace_id`, `public_key`, `environment`, `transaction`, `sampled`, `sample_rate`. A `client_report` envelope has an empty header `{}`.

## Item types

| `type` | Item header | Payload | Action |
|---|---|---|---|
| `event` | `{"type":"event"}` | one event JSON | **store and group** |
| `log` | `item_count`, `content_type: application/vnd.sentry.items.log+json` | `{"items":[...]}` batch | **store** |
| `span` | `item_count`, `content_type: application/vnd.sentry.items.span.v2+json` | `{"items":[...]}` batch | accept and drop (tracing is out of scope) |
| `client_report` | `{"type":"client_report"}` | `{timestamp, discarded_events:[{reason, category, quantity}]}` | accept and drop (maybe count) |
| `transaction`, `session`, `sessions`, `attachment`, `profile`, `replay_*`, `check_in`, ... | | | accept and drop |

SDK 11 sends tracing as `span` (v2) items, not `transaction` events.

## Event payload

Top-level keys seen: `event_id`, `timestamp`, `level`, `platform`, `environment`, `server_name`, `transaction`, `exception`, `message`, `fingerprint`, `tags`, `user`, `request`, `contexts`, `breadcrumbs`, `modules`, `sdk`.

- **`exception.values[]`:** `{type, value, mechanism {type, handled}, stacktrace {frames[]}}`.
  - Frames are ordered **oldest first** (the crashing frame is last).
  - Frame fields seen (Node): `filename`, `module`, `function`, `lineno`, `colno`, `in_app`, `pre_context`, `context_line`, `post_context`, `vars`. Browser frames have only `filename`, `function`, `lineno`, `colno`, `in_app`.
- **Error `cause` chain:** several entries in `exception.values`, **innermost cause first**. The outer error is last, with `mechanism.type: "chained"` on inner ones.
- **`captureMessage`:** has `message` (string) and also an `exception` value **without `type`**, with a synthetic stack trace.
- **`fingerprint`:** an array of strings, present only when set by the app. It overrides default grouping.
- **`mechanism`:**
  - `handled: false` for crashes (`auto.http.express`, `auto.browser.global_handlers.onerror` / `onunhandledrejection`);
  - `generic` with `handled: true` for `captureException`.
- **`contexts`:** `trace {trace_id, span_id, parent_span_id}`, `runtime`, `os`, `device`, `app`, `culture`, `cloud_resource`, plus custom contexts (`setContext`).
- **`breadcrumbs`:** a plain array `[{timestamp, category, level, message, data}]`. It includes console output captured before the error.
- **`user`:** `{id, email, username, ip_address}`.
- **`request`:** `{method, url, headers, cookies}`.

Browser events have no `server_name`/`modules`. Their frames point at the page/bundle URL (minified in production, so they need source maps).

### SDK-side filtering

The SDK drops some events itself. For example, `dedupeIntegration` drops an identical error sent twice in a row, and the SDK reports this in a `client_report` (`reason: event_processor`). To test server grouping, send identical errors non-consecutively.

## Log payload

```json
{
  "timestamp": 1794328021.0768,
  "level": "warn",
  "body": "pino warn: slow query",
  "trace_id": "bd916706b6ca4a468b9ab42bb321b9c9",
  "severity_number": 13,
  "attributes": {
    "orderId":        { "value": 1001, "type": "integer" },
    "sentry.origin":  { "value": "auto.log.pino", "type": "string" },
    "sentry.environment": { "value": "development", "type": "string" },
    "server.address": { "value": "test-host", "type": "string" }
  }
}
```

- Attributes are typed (`string`, `integer`, `double`, `boolean`).
- `sentry.origin` tells the source: `auto.log.pino`, `auto.log.console`, or absent for `Sentry.logger`.
- Logs are batched and flushed about every 5 s.
- `trace_id` links a log to events with the same `contexts.trace.trace_id`.
- Bodies can contain ANSI color codes (pino/AdminForth), so strip or render them in the UI.
- Frameworks may log the same error that is also sent as an event. For example, AdminForth logs every 500 via pino. Link these by `trace_id` rather than treating them as separate problems.

## Source maps (to research in the source maps stage)

Production browser frames need source maps. `sentry-cli` / the bundler plugins:

1. inject a `debugId` into each bundle and its map;
2. upload them through the Sentry API with a bearer auth token. This is a chunked upload: get upload options, POST chunks, then call an "assemble" endpoint for an artifact bundle and poll its status.

Events then reference bundles via `debug_meta.images[]` with `debug_id`. The exact endpoints and formats still need to be checked against `sentry-cli` source before implementation. No `debug_meta` was observed in our samples, because the test page used inline scripts.
