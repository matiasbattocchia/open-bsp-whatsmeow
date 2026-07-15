# open-bsp-whatsmeow

Self-hosted WhatsApp Web bridge for [OpenBSP](https://github.com/matiasbattocchia/open-bsp-api)
(`whatsapp-web` service). A thin, stateless wrapper around
[whatsmeow](https://github.com/tulir/whatsmeow) that adapts to OpenBSP's
native connector contract — the bridge speaks OpenBSP's own message format,
not the other way around.

> **Unofficial WhatsApp.** This uses the WhatsApp Web multidevice protocol,
> not the Cloud API: pairing via QR/code, ban risk applies, no templates and
> no business features. See open-bsp-api's `MIGRATING_FROM_WHATSAPP_WEB_JS.md`
> for the trade-offs.

## Architecture

```
                    ┌────────────── OpenBSP (Supabase) ──────────────┐
open-bsp-whatsmeow ─►  whatsapp-web-webhook     (inbound messages)   │
   (this, Go)      ◄─  whatsapp-web-dispatcher  (outbound, /dispatch)│
                   ◄─► whatsapp-web-management  (pairing, lifecycle) │
                    │  Postgres: lends the `whatsmeow` schema        │
                    └────────────────────────────────────────────────┘
```

- **Stateless container** — sessions (Signal keys, device state) live in the
  `whatsmeow` schema of the Postgres pointed to by `DATABASE_URL`. OpenBSP
  never reads that schema; kill/update/restart the container freely.
- **No Supabase credentials** — the bridge only holds the shared
  `BRIDGE_TOKEN` and talks to the three edge functions over HTTP.
- **One replica by design** — a WhatsApp session is a single WebSocket.
- **Posts everything** — own sends echo back and are deduped by
  `external_id` upsert; phone-sent messages become outgoing rows.

## Configuration

| Env             | Required | Description                                             |
| --------------- | -------- | ------------------------------------------------------- |
| `DATABASE_URL`  | yes      | Postgres DSN; `search_path=whatsmeow` appended if absent |
| `OPENBSP_URL`   | yes      | Edge functions base, e.g. `http://kong:8000/functions/v1` |
| `BRIDGE_TOKEN`  | yes      | Shared bearer token (must match `WHATSAPP_WEB_BRIDGE_TOKEN` in OpenBSP) |
| `LISTEN_ADDR`   | no       | Default `:8081`                                          |
| `LOG_LEVEL`     | no       | Default `INFO`                                           |

OpenBSP side (`supabase/functions/.env`): set `WHATSAPP_WEB_URL` to this
service's base URL and `WHATSAPP_WEB_TOKEN` to the same token.

## docker-compose

```yaml
services:
  whatsmeow-bridge:
    image: ghcr.io/matiasbattocchia/open-bsp-whatsmeow
    environment:
      DATABASE_URL: postgres://postgres:postgres@db:5432/postgres
      OPENBSP_URL: http://kong:8000/functions/v1
      BRIDGE_TOKEN: change-me
    ports: ["8081:8081"]
```

## HTTP API (server-to-server only, bearer `BRIDGE_TOKEN`)

- `POST /dispatch` — called by `whatsapp-web-dispatcher`;
  `{type: "message"|"status", record, media_url?}` → `{external_id, status}`.
  4xx = permanent failure, 5xx = transient (retried by OpenBSP's cron).
- `POST /sessions` — `{organization_id, phone_number?}` →
  `{qr_code}` or `{pairing_code}`.
- `GET /sessions/{address}` — `{address, connected, logged_in}`.
- `DELETE /sessions/{address}` — logout + delete device.

## Status / TODO (v0 scaffold)

- [x] Text messages in/out, receipts, contact pushnames, QR + phone-code
      pairing, logout, logged-out notification
- [ ] Media (`DownloadAny` → `POST /media`; `media_url` → `Upload`)
- [ ] Groups metadata (`GetGroupInfo` → conversation name) — group text
      messages already flow
- [ ] History sync import (explicit final statuses; never `pending`)
- [ ] Edits/revokes translation (webhook contract already supports them)
- [ ] LID → phone canonicalization for LID-only contacts
- [ ] QR rotation surface (poll/SSE) for the pairing UI

## Development

```bash
go build ./...   # or: docker build .
```
