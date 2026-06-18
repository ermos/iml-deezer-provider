# 🎵 deezer-http

> A tiny HTTP proxy that searches the **Deezer** catalog for artists, albums and
> tracks — and nothing more. 🔍

It speaks the gossignol generic HTTP provider
protocol: gossignol calls a few fixed `GET` endpoints, this service forwards the
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

Every request must carry the `X-Deezer-Arl` header (gossignol sends it for you).
Without it you get `401`.

| Method & path | Query | Returns |
| ------------- | ----- | ------- |
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
the gossignol server can reach. The only secret (the ARL) is supplied by
gossignol through the header. 🔒

### 🐳 Docker

```bash
docker build -t deezer-http .
docker run -p 8080:8080 deezer-http
```

See [`Dockerfile`](./Dockerfile). 📦 Pushes to `main` auto-build and publish
`ghcr.io/gossignol/iml-deezer-provider` with an auto-bumped semver tag (see
[`.github/workflows/release.yml`](./.github/workflows/release.yml)), so you can
also just pull it:

```bash
docker run -p 8080:8080 ghcr.io/gossignol/iml-deezer-provider:latest
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
      - "8080"        # internal-only; reachable by the gossignol service below
    restart: unless-stopped

  gossignol:
    image: ghcr.io/gossignol/gossignol:latest
    ports:
      - "4533:4533"
    depends_on:
      - deezer-http
```

## ⚙️ Register it in gossignol

gossignol forwards the `headers` from the provider config **verbatim on every
request** — that's how the ARL gets in. Register the provider via the admin API:

```bash
curl -X POST http://localhost:4533/api/v1/admin/providers \
  -H 'Authorization: Bearer <admin-token>' \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "deezer",
    "kind": "http",
    "endpoint": "http://deezer-http:8080",
    "config": "{\"headers\":{\"X-Deezer-Arl\":\"<your-deezer-arl>\"}}",
    "enabled": true
  }'
```

- `endpoint` is the URL gossignol reaches this service on (e.g. the compose service
  name `http://deezer-http:8080`).
- `config` is a **JSON string** (note the escaped quotes); put your ARL in the
  `X-Deezer-Arl` header.
- Want to serve several Deezer accounts? Register the provider more than once with
  a different `name` and a different ARL in the header. 👥

That's it. Search results from Deezer now flow into the gossignol catalog. 🎉
