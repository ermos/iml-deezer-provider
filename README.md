# 🎵 deezer-http

> A tiny HTTP proxy that searches the **Deezer** catalog for artists, albums and
> tracks — and nothing more. 🔍

It speaks a generic HTTP provider
protocol: the host server calls a few fixed `GET` endpoints, this service forwards the
query to Deezer and hands back clean JSON. That's the whole job. 🤏

- 🧠 **Metadata first** — search, resolve, artist/album browsing.
- 🎧 **Plus a `/download`** passthrough that streams the track straight through
  (nothing is ever written to disk).
- 🔑 **No env secret** — the Deezer account ARL travels per-request in the
  `X-Deezer-Arl` header, so one instance can serve several accounts.
- 🪶 **Stateless & boring** — one small Go binary, no database, no config files.

## ⚠️ Your content, your responsibility

This is just a mechanism. **You** are solely responsible for ensuring you have
the legal right to access and use whatever you connect it to, and for complying
with Deezer's terms and the copyright laws in your jurisdiction.

## 🔌 Endpoints

Every request must carry the `X-Deezer-Arl` header (the host sends it for you).
Without it you get `401`.

| Method & path | Query | Returns |
| ------------- | ----- | ------- |
| `GET /capabilities`  | —            | `{"version":1,"name":"deezer","config":{…}}` (no ARL needed) |
| `GET /search`        | `q`, `limit` | `{"results":[<track>]}` |
| `GET /resolve`       | `id`         | `{"result":<track>}` |
| `GET /download`      | `id`         | raw audio bytes 🎧 |
| `GET /artists`       | `q`, `limit` | `{"artists":[<artist>]}` |
| `GET /artist/albums` | `id`, `limit`| `{"albums":[<album>]}` |
| `GET /artist/tracks` | `id`, `limit`| `{"results":[<track>]}` |
| `GET /album/tracks`  | `id`, `limit`| `{"results":[<track>]}` |
| `GET /artist/image`  | `name`       | `{"imageUrl":"…"}` |
| `GET /healthz`       | —            | `{"status":"ok"}` (no ARL needed) |

## 🚀 Run

```bash
go run .                 # listens on :8080
ADDR=:9000 go run .      # custom address
DEEZER_QUALITY=FLAC go run .
```

| Env | Default | Purpose |
| --- | ------- | ------- |
| `ADDR` | `:8080` | listen address |
| `DEEZER_QUALITY` | `MP3_128` | `FLAC` \| `MP3_320` \| `MP3_128` (download quality) |

The service is unauthenticated at the network layer — run it on a private network
the host server can reach. The only secret (the ARL) is supplied by
the host through the header. 🔒

### 🐳 Docker

```bash
docker build -t deezer-http .
docker run -p 8080:8080 deezer-http
```

See [`Dockerfile`](./Dockerfile). 📦 Pushes to `main` auto-build and publish
`ghcr.io/ermos/iml-deezer-provider` with an auto-bumped semver tag (see
[`.github/workflows/release.yml`](./.github/workflows/release.yml)), so you can
also just pull it:

```bash
docker run -p 8080:8080 ghcr.io/ermos/iml-deezer-provider:latest
```

### 🧩 docker-compose

```yaml
services:
  deezer-http:
    build: .
    # or: image: ghcr.io/you/deezer-http:latest
    environment:
      DEEZER_QUALITY: MP3_320
    expose:
      - "8080"        # internal-only; reachable by the host service below
    restart: unless-stopped

  host-server:
    image: your-host-server:latest
    ports:
      - "4533:4533"
    depends_on:
      - deezer-http
```

## ⚙️ Register it on the host server

Registration is the three-step admin flow — you never type the name (the host
reads it from `/capabilities`). The host forwards the `header` from the provider
config **verbatim on every request** — that's how the ARL gets in.

> ℹ️ The config key is `headers` (plural). The singular `header` is still accepted
> for backward compatibility but is deprecated — prefer `headers`.

**1. Create from the URL.** The host calls `/capabilities`, takes `name: "deezer"`,
and creates the provider **disabled**:

```bash
curl -X POST http://localhost:4533/api/v1/admin/providers \
  -H 'Authorization: Bearer <admin-token>' -H 'Content-Type: application/json' \
  -d '{ "endpoint": "http://deezer-http:8080" }'
```

**2. Fill the config** with your ARL. `config` is a **JSON string**; the `X-Deezer-Arl`
header is the one required field declared by `/capabilities`:

```bash
curl -X POST http://localhost:4533/api/v1/admin/providers \
  -H 'Authorization: Bearer <admin-token>' -H 'Content-Type: application/json' \
  -d '{
    "name": "deezer",
    "kind": "http",
    "endpoint": "http://deezer-http:8080",
    "config": "{\"headers\":{\"X-Deezer-Arl\":\"<your-deezer-arl>\"}}"
  }'
```

**3. Enable it** (re-runs the capability check, so an incomplete config can't go live):

```bash
curl -X PUT http://localhost:4533/api/v1/admin/providers/deezer/enabled \
  -H 'Authorization: Bearer <admin-token>' -H 'Content-Type: application/json' \
  -d '{ "enabled": true }'
```

- `endpoint` is the URL the host reaches this service on (e.g. the compose service
  name `http://deezer-http:8080`).
- Want to serve several Deezer accounts? Register more than once with a different
  ARL in the header (the host stores each under the same `deezer` name slug, so
  use separate host instances or names as your host allows). 👥

That's it. Search results from Deezer now flow into the host's catalog. 🎉
