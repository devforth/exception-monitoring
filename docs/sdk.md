# Connecting apps

The server speaks the protocol of the official [Sentry SDKs](https://docs.sentry.io/platforms/), so an app is connected by installing the SDK of its platform and giving it a DSN. Nothing else changes in the app.

Each service (backend, frontend, worker, …) should be its own project: the UI shows which project an error came from, and notifications, levels and retention can be set per project. Copy the DSN from the **Projects** page (a coding agent connected over [MCP](mcp.md) gets it from `list_projects`):

```
https://<public_key>@errors.example.com/<project id>
```

Tested with the Node.js and browser SDKs, version 11. Other SDKs send the same envelope format and should work; please report one that does not.

## Node.js

```sh
npm install @sentry/node
```

Initialize it before anything else is imported (e.g. a file loaded with `node --import ./instrument.mjs`):

```js
import * as Sentry from "@sentry/node";

Sentry.init({
  dsn: process.env.SENTRY_DSN,
  environment: process.env.NODE_ENV,
  // Send logs: Sentry.logger.info(...), and console/pino output with the integrations below.
  enableLogs: true,
  // Tracing ties logs to the request that failed; the spans themselves are not stored.
  tracesSampleRate: 1.0,
  integrations: [
    Sentry.consoleLoggingIntegration({ levels: ["log", "warn", "error"] }),
    // Sentry.pinoIntegration(),
  ],
});
```

Unhandled errors are sent automatically; handled ones with `Sentry.captureException(error)`, messages with `Sentry.captureMessage("text", "warning")`.

The SDK's `Dedupe` integration drops an error identical to the previous one, however much later it happens, so repeats are not counted. The server groups repeats into one issue and has its own rate limit, so you may turn it off:

```js
integrations: (defaults) => defaults.filter((integration) => integration.name !== "Dedupe"),
```

## Browser

```sh
npm install @sentry/browser
```

```js
import * as Sentry from "@sentry/browser";

Sentry.init({
  dsn: "https://<public_key>@errors.example.com/2",
  enableLogs: true,
  integrations: [Sentry.browserTracingIntegration()],
  tracesSampleRate: 1.0,
});
```

Browsers send errors straight to the server, so:

- set `ALLOWED_ORIGINS` to the sites that send them (`https://app.example.com`), or leave `*`;
- the ingest endpoint must be reachable from your users' browsers, even if the UI is limited with `ALLOWED_IPS`.

A DSN key is public by design (it is in the page); it only allows sending events to its project.

Ad blockers often block requests to error trackers. The SDK's `tunnel` option sends them through your own backend instead; forward the request body unchanged to `https://errors.example.com/api/<project id>/envelope/`, and the server reads the key from the envelope.

Production bundles are minified; source maps are not supported yet, so frames show the bundle's names.

## Other platforms

Python:

```python
import sentry_sdk

sentry_sdk.init(dsn="https://<public_key>@errors.example.com/3", enable_logs=True)
```

Go, PHP, Ruby, Java, .NET and the rest: follow the SDK's own guide and use the DSN from the Projects page.

## What is stored

- **Events** (errors and messages) of the levels the project records, by default `error` and `fatal`; set `RECORD_EVENT_LEVELS` or the project's levels to keep warnings or messages too. Events are grouped into **issues**.
- **Logs** of every level by default (`RECORD_LOG_LEVELS`).
- Tracing spans, transactions, session replays, profiles, attachments and client reports are accepted and dropped.

Before anything is stored, values under names like password, token, secret, cookie, session or authorization are removed, as are credentials in URLs and headers. Large parts that do not help a fix (library source code, module lists, more than 50 breadcrumbs) are dropped too.

## Grouping

Events with the same fingerprint in a project are one issue. The default fingerprint is built from the exception types and the in-app stack frames: file, function and source line, without line numbers, so a deploy that moves code does not open new issues. Messages and errors without a usable stack are grouped by their text, with numbers and IDs ignored ("order 890" and "order 891" match).

To group differently, set the fingerprint in the SDK:

```js
Sentry.withScope((scope) => {
  scope.setFingerprint(["payment-gateway-timeout"]);          // all such errors in one issue
  // scope.setFingerprint(["{{ default }}", tenantId]);       // the default, split per tenant
  Sentry.captureException(error);
});
```

A resolved issue that happens again is reopened (a regression) and notifies again; an ignored one stays ignored.

## Logs and errors

On an issue page, the logs written while handling the failed request are shown with the error. They are matched by the trace and span IDs the SDK attaches when tracing is on; without them the page falls back to logs of the same trace from the minute around the error. The trace ID on the issue page opens every log of that trace on the **Logs** page.
