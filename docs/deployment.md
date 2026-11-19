# Deployment

The server is one container, `devforth/exception-monitoring`, for amd64 and arm64. It keeps all its data in an embedded database in `/data` and needs nothing else. The database is the directory `/data/leveldb`; without `SECRET_KEY`, the key that encrypts chat tokens is `/data/secret.key`.

Image tags: `latest` (newest release), `X.Y` (newest patch of a minor version), `X.Y.Z`. Pin `X.Y` or `X.Y.Z` in production and upgrade on purpose.

## Docker Compose

1. Copy [compose.yml](../compose.yml) to the server.
2. Next to it, create `.env` (template: [.env.example](../.env.example), all settings: [Configuration](configuration.md)). A minimal one:

   ```sh
   PUBLIC_URL=https://errors.example.com
   ADMIN_PASSWORD=<at least 12 characters>
   SECRET_KEY=<openssl rand -hex 32>   # encrypts chat tokens; generated into /data when not set
   TRUSTED_PROXIES=172.16.0.0/12      # the proxy, see below
   ```

3. `docker compose up -d`, then open the UI and log in as `admin`.
4. Add projects on the **Projects** page (or with `PROJECTS`) and give their DSNs to your apps: [Connecting apps](sdk.md).

The container runs as an unprivileged user (uid 65534) without a shell. A bind-mounted data directory must be writable by it: `sudo chown 65534:65534 ./data`. A named volume, as in the example, needs nothing.

Docker checks the server with its health check (`docker ps` shows `healthy`); a load balancer can use `GET /health`, which answers 200 while the database is reachable.

## HTTPS and a reverse proxy

Use HTTPS whenever the server is reachable from other machines: users log in to the UI and coding agents send their tokens. Put it behind the reverse proxy you already have, publish the port only on localhost (`"127.0.0.1:8000:8000"` in `compose.yml`) and set:

- `PUBLIC_URL=https://errors.example.com`, so DSNs, links and the MCP address use it.
- `TRUSTED_PROXIES` to the address the proxy connects from: `127.0.0.1` for a proxy on the host network, or the Docker network (`docker network inspect` shows its subnet, often in `172.16.0.0/12`) for a proxy in a container. Without it the server sees every request coming from the proxy, so `ALLOWED_IPS` and the login limit cannot tell clients apart, and pages are treated as plain HTTP.

The proxy must pass `Host`, `X-Forwarded-For` and `X-Forwarded-Proto`, and allow request bodies up to 20 MB (SDKs send large envelopes).

Caddy (sets the headers itself, gets the certificate):

```
errors.example.com {
	request_body {
		max_size 20MB
	}
	reverse_proxy 127.0.0.1:8000
}
```

nginx:

```nginx
server {
    listen 443 ssl;
    server_name errors.example.com;
    # ssl_certificate ...

    client_max_body_size 20m;

    location / {
        proxy_pass http://127.0.0.1:8000;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

## Securing it

- The ingest endpoint (`/api/<project id>/envelope/`) must stay reachable by your apps, and by browsers if you monitor a frontend. It accepts only requests with a project's key, limited by `RATE_LIMIT_PER_MINUTE`; `ALLOWED_ORIGINS` limits which sites a browser may send from.
- Everything else (UI, REST API, MCP) can be restricted to your office or VPN with `ALLOWED_IPS`.
- Passwords, cookies, tokens and similar values are removed from incoming events and logs before they are stored.
- Users get one of three roles: **superadmin** (everything), **admin** (projects, notifications, users, deleting issues) and **user** (issues, logs, DSNs, their own coding agents). Add them on the **Users** page; each gets a temporary password to change at the first login.

## Data

Events and logs are kept until the database nears [its size limit](configuration.md#retention). What takes effort to set up again lives in the same database: projects and their keys (keep them in `PROJECTS` and the DSNs survive any loss), users, notification connections and connected agents. Keep the `data` volume on a disk you trust, and back it up.

## Backup and restore

```sh
docker compose run --rm exception-monitoring backup
```

The running server writes a consistent copy to `/data/backups/<time>` without stopping (with the server stopped, the command reads the database itself); add an absolute path for another place, a new or empty directory. The copy holds `leveldb/` and `secret.key`, which decrypts the chat tokens of notifications (with `SECRET_KEY` set there is no file: keep that value). While a backup runs, the cleanup by size waits. `/data/backups` is on the database's disk: copy backups off (`docker compose cp exception-monitoring:/data/backups ./backups`) and delete them there with the helper container below (`rm -rf /data/backups`). Never copy `/data/leveldb` of a running server by hand: such a copy may not open.

To restore, stop the server and put the backup's `leveldb/` (and `secret.key`, if it has one) in place of those in the volume (the image has no shell, so use a helper container; `docker volume ls` shows the volume's name):

```sh
docker compose stop exception-monitoring
docker run --rm -v <project>_data:/data -v "$PWD/backups/<date>:/backup:ro" alpine \
  sh -c 'rm -rf /data/leveldb && cp -a /backup/. /data/ && chown -R 65534:65534 /data'
docker compose start exception-monitoring
```

## Upgrades

```sh
docker compose pull && docker compose up -d
```

On shutdown (`docker stop` sends SIGTERM) the server finishes requests and writes queued notifications to the database; they are sent after the next start.

## Forgotten password

The running server holds the database, so stop it first:

```sh
docker compose stop exception-monitoring
docker compose run --rm exception-monitoring reset-password          # the superadmin
docker compose run --rm exception-monitoring reset-password <username>
docker compose start exception-monitoring
```

It prints a temporary password; the user chooses a new one at the next login. Their sessions end; their connected agents keep working.

## Logs

`docker compose logs -f` shows the startup settings (enabled notification types, the size limit), warnings about missing settings and errors. Set `LOG_LEVEL=debug` to see every request and why an envelope was rejected (wrong key, unknown project, rate limit), and `LOG_FORMAT=json` for a log collector.
