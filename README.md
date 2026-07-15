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
| `BRIDGE_TOKEN`  | yes      | Shared bearer token (must match `WHATSAPP_WEB_TOKEN` in OpenBSP) |
| `LISTEN_ADDR`   | no       | Default `:$PORT` (PaaS convention) or `:8081`            |
| `LOG_LEVEL`     | no       | Default `INFO`                                           |

OpenBSP side (`supabase/functions/.env`): set `WHATSAPP_WEB_URL` to this
service's base URL and `WHATSAPP_WEB_TOKEN` to the same token.

## Deployment

The reference deployment runs on Zeabur (project *OpenBSP*) at
`https://whatsmeow.openbsp.dev`, built from this repo's Dockerfile; the
OpenBSP edge functions reach it via the `WHATSAPP_WEB_URL` /
`WHATSAPP_WEB_TOKEN` secrets. On hosted Supabase, point `DATABASE_URL` at
Supavisor (transaction mode, port 6543).

Self-hosters can use docker-compose (no published image yet — build from
source):

```yaml
services:
  whatsmeow-bridge:
    build: https://github.com/matiasbattocchia/open-bsp-whatsmeow.git
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
  `{session_id, status: "pending", qr_code?}` or `{..., pairing_code?}`.
- `GET /sessions/pending/{session_id}` — poll during pairing (QR codes
  rotate ~20s): `{session_id, status: pending|paired|error, qr_code?,
  pairing_code?, address?, error?}`.
- `GET /sessions/{address}` — `{address, connected, logged_in}`.
- `DELETE /sessions/{address}` — logout + delete device.

## Status / TODO (v0)

Working end to end:

- Text messages in/out (echoes included: phone-sent messages become
  outgoing rows, bridge-sent ones dedupe on `external_id`)
- Media in/out (image, audio, video, document, sticker). Inbound:
  `DownloadAny()` (fetch+decrypt) → webhook `/media` → FilePart with the
  returned `internal://` URI; on failure the message is preserved with an
  error status. Outbound: GET the signed `media_url` from the dispatcher →
  `Upload()` (encrypt+push to WhatsApp CDN) → per-kind protobuf, enforcing
  WhatsApp's per-type size caps (oversize = permanent 422).
- Delivery/read receipts in; read receipts out (`MarkRead`)
- Contact pushnames
- QR + phone-code pairing with rotation polling, logout, session-death
  (`logged_out`) notification to management

Not yet implemented:

- [ ] DataParts (location, contacts, ...) — rejected with 422 → OpenBSP
      marks the message failed
- [ ] Typing indicators out (`SendChatPresence`) — a forwarded typing
      status is currently treated as a read receipt
- [ ] Groups metadata (`GetGroupInfo` → conversation name) — group text
      messages already flow, with per-message sender
- [ ] History sync import (explicit final statuses; never `pending`)
- [ ] Edits/revokes translation (webhook contract already supports them)
- [ ] LID → phone canonicalization for LID-only contacts

## Development

```bash
go build ./...   # or: docker build .
```
