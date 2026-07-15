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
| `DATABASE_URL`  | yes      | Postgres DSN; `search_path=whatsmeow` and `default_query_exec_mode=simple_protocol` appended if absent (the latter is required behind transaction-mode poolers like Supavisor 6543) |
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
- Reactions, locations, and contact cards (vCard) in/out — same DataPart
  shapes as the Cloud API service
- Replies (`re_message_id` ↔ quoted message) in/out; edits and revokes in
- Delivery/read receipts in; read receipts + typing indicators out
  (`MarkRead`, `SendChatPresence`)
- Contact pushnames
- QR + phone-code pairing with rotation polling, logout, session-death
  (`logged_out`) notification to management

Parity notes vs the `whatsapp` (Cloud API) service:

- Templates are a Cloud API concept with no WhatsApp Web equivalent —
  template sends fail permanently (422) with an explicit error.
- External ids are `wmw.<own>.<chat>.<sender>.<id>`: the sender segment
  encodes direction (sender == own) and the group participant, so
  reactions and quotes reconstruct the full WhatsApp MessageKey with no
  OpenBSP lookup — including quotes in groups.
- WhatsApp Status (stories) and newsletters are dropped — they are not
  conversations.
- History media is imported as metadata only (old media is frequently gone
  from WhatsApp's CDN): FileParts without a URI render as unavailable
  attachments. History rows always carry explicit final statuses — never
  `pending`, which is OpenBSP's automation gate.
- LID-only peers the store has no phone mapping for fall back to the LID
  digits as contact_address (rare; the mapping fills in as messages flow).

Also working: group subjects → conversation names (on first sight and on
renames), history sync import (messages + pushnames, chunked), LID → phone
canonicalization for contact addresses.

## Development

```bash
go build ./...   # or: docker build .
```
