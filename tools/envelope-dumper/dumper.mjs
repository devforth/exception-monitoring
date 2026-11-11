// Research tool: pretends to be a Sentry ingest server and dumps every request
// (headers + decompressed body + parsed envelope items) to disk.
//
// Usage: node tools/envelope-dumper/dumper.mjs [outDir]   (PORT env, default 8000)
// Raw dumps go to <repo>/envelopes/ (gitignored). Sanitize before copying to testdata/.

import http from "node:http";
import fs from "node:fs";
import path from "node:path";
import zlib from "node:zlib";
import crypto from "node:crypto";

const PORT = Number(process.env.PORT ?? 8000);
const REPO_ROOT = path.resolve(path.dirname(new URL(import.meta.url).pathname), "../..");
const OUT_DIR = path.resolve(process.argv[2] ?? path.join(REPO_ROOT, "envelopes"));
fs.mkdirSync(OUT_DIR, { recursive: true });

let seq = fs.readdirSync(OUT_DIR).filter((f) => f.endsWith(".json")).length;

const CORS_HEADERS = {
  "Access-Control-Allow-Origin": "*",
  "Access-Control-Allow-Methods": "POST, OPTIONS",
  "Access-Control-Allow-Headers": "*",
  "Access-Control-Expose-Headers": "x-sentry-rate-limits, retry-after",
};

function decompress(buf, encoding) {
  switch ((encoding ?? "").toLowerCase()) {
    case "gzip": return zlib.gunzipSync(buf);
    case "deflate": return zlib.inflateSync(buf);
    case "br": return zlib.brotliDecompressSync(buf);
    default: return buf;
  }
}

// Envelope = header line, then (item header line + payload) pairs.
// Payload length comes from item header `length` if present, otherwise up to the next "\n".
function parseEnvelope(buf) {
  let pos = 0;
  const readLine = () => {
    const end = buf.indexOf(0x0a, pos);
    const line = buf.subarray(pos, end === -1 ? buf.length : end);
    pos = end === -1 ? buf.length : end + 1;
    return line;
  };
  const header = JSON.parse(readLine().toString("utf8"));
  const items = [];
  while (pos < buf.length) {
    const headerLine = readLine();
    if (headerLine.length === 0) continue;
    const itemHeader = JSON.parse(headerLine.toString("utf8"));
    let payload;
    if (typeof itemHeader.length === "number") {
      payload = buf.subarray(pos, pos + itemHeader.length);
      pos += itemHeader.length;
      if (buf[pos] === 0x0a) pos += 1;
    } else {
      payload = readLine();
    }
    const text = payload.toString("utf8");
    let parsed;
    try { parsed = JSON.parse(text); } catch { parsed = { _nonJson: true, size: payload.length } }
    items.push({ header: itemHeader, payload: parsed });
  }
  return { header, items };
}

const server = http.createServer((req, res) => {
  if (req.method === "OPTIONS") {
    res.writeHead(204, CORS_HEADERS);
    return res.end();
  }

  const chunks = [];
  req.on("data", (c) => chunks.push(c));
  req.on("end", () => {
    const raw = Buffer.concat(chunks);
    const url = new URL(req.url, `http://${req.headers.host}`);
    const record = {
      receivedAt: new Date().toISOString(),
      method: req.method,
      path: url.pathname,
      query: Object.fromEntries(url.searchParams),
      headers: req.headers,
      rawSize: raw.length,
    };

    let body;
    try {
      body = decompress(raw, req.headers["content-encoding"]);
      record.decompressedSize = body.length;
    } catch (e) {
      record.decompressError = String(e);
      body = raw;
    }

    let itemTypes = ["unknown"];
    try {
      record.envelope = parseEnvelope(body);
      itemTypes = record.envelope.items.map((i) => i.header.type);
    } catch (e) {
      record.parseError = String(e);
    }

    seq += 1;
    const project = url.pathname.match(/^\/api\/([^/]+)\//)?.[1] ?? "x";
    const name = `${String(seq).padStart(3, "0")}-p${project}-${[...new Set(itemTypes)].join("+")}`;
    fs.writeFileSync(path.join(OUT_DIR, `${name}.json`), JSON.stringify(record, null, 2));
    fs.writeFileSync(path.join(OUT_DIR, `${name}.envelope`), body);

    console.log(`${record.receivedAt} ${req.method} ${url.pathname} ${req.headers["content-encoding"] ?? "-"} ${raw.length}B -> ${name}`);

    const eventId = record.envelope?.header?.event_id ?? crypto.randomUUID().replace(/-/g, "");
    res.writeHead(200, { ...CORS_HEADERS, "Content-Type": "application/json" });
    res.end(JSON.stringify({ id: eventId }));
  });
});

server.listen(PORT, () => console.log(`envelope dumper on http://localhost:${PORT}, writing to ${OUT_DIR}`));
