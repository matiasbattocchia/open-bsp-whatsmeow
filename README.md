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
  database `DATABASE_URL` points at: the `whatsmeow` schema of a Postgres
  (OpenBSP never reads that schema), or an embedded SQLite file when there is
  no database to lend. Either way the container is disposable —
  kill/update/restart it freely; the state is not in it.
- **No Supabase credentials** — the bridge only holds the shared
  `BRIDGE_TOKEN` and talks to the three edge functions over HTTP.
- **One replica by design** — a WhatsApp session is a single WebSocket.
- **Posts everything** — own sends echo back and are deduped by
  `external_id` upsert; phone-sent messages become outgoing rows.

## Configuration

| Env             | Required | Description                                             |
| --------------- | -------- | ------------------------------------------------------- |
| `DATABASE_URL`  | yes      | Database DSN; the engine follows the scheme. `postgres://…` — `search_path=whatsmeow` and `default_query_exec_mode=simple_protocol` appended if absent (the latter is required behind transaction-mode poolers like Supavisor 6543). `file:…` / `sqlite:…` — embedded SQLite, no server to run; `foreign_keys`, `journal_mode=WAL` and `busy_timeout` pragmas appended if absent. Put the file on a persistent volume: it holds the session |
| `OPENBSP_URL`   | no       | Where a session delivers when its pairing named no `webhook_url`, e.g. `http://kong:8000/functions/v1`. Unset ⇒ every `POST /sessions` must name one |
| `BRIDGE_TOKEN`  | yes      | Shared bearer token (must match `WHATSAPP_WEB_TOKEN` in OpenBSP) |
| `LISTEN_ADDR`   | no       | Default `:$PORT` (PaaS convention) or `:8081`            |
| `LOG_LEVEL`     | no       | Default `INFO`                                           |
| `LINK_EVENTS`   | no       | `true` also posts a paired session's link state to the management function: `disconnected` when its socket drops, `connected` when whatsmeow has it back. Default `false` — OpenBSP's management function accepts only the pairing `connected` and `logged_out` |

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
      # …or, with no Postgres to lend — the file is the session, so it must
      # outlive the container:
      # DATABASE_URL: file:/data/whatsmeow.db
      OPENBSP_URL: http://kong:8000/functions/v1
      BRIDGE_TOKEN: change-me
    # volumes: ["whatsmeow-data:/data"]
    ports: ["8081:8081"]
```

## HTTP API (server-to-server only, bearer `BRIDGE_TOKEN`)

- `POST /dispatch` — called by `whatsapp-web-dispatcher`;
  `{type: "message"|"status", record, media_url?}` → `{external_id, status}`.
  4xx = permanent failure, 5xx = transient (retried by OpenBSP's cron).
  `{type: "contact", record, contact: {name, remove}}` writes the address
  book: the record's `conversation_address` is the person, `name` what they
  are saved as (empty ⇒ the name the wire knows them by), `remove` takes the
  entry out. Answers `{status: "sent", name}` once WhatsApp has the patch —
  the name it carried, so a nameless save says what it settled on. The book
  itself stays WhatsApp's: what the consumer sees of it is `sender_saved` on
  every later message from that person.
- `POST /sessions` — `{organization_id, phone_number?, agent_id?, webhook_url?}` →
  `{session_id, status: "pending", qr_code?}` or `{..., pairing_code?}`.
  `webhook_url` is where THIS session's traffic (webhook batches, media
  uploads, session events, relative `media_url` fetches) goes, kept with its
  mapping; absent ⇒ `OPENBSP_URL`. One bridge can serve many consumers.
- `GET /sessions/pending/{session_id}` — poll during pairing (QR codes
  rotate ~20s): `{session_id, status: pending|paired|error, qr_code?,
  pairing_code?, address?, error?}`.
- `GET /sessions/{address}` — `{address, connected, logged_in}`.
- `GET /contacts/{address}?q=…` — the address book's read side:
  `{contacts: [{address, extra: {name}}]}`, the entries matching `q`. A name
  matches case-insensitively on a substring and digits match the address, so
  `+54 9 261 610-4507` finds whoever wears that number. Only entries the
  ACCOUNT named are answered — the same ones `sender_saved` marks a message
  with — so somebody known to the wire by pushname alone is not in the book.
  `q` is required (422 without it) and 25 entries is the most one lookup
  answers: this is a lookup, not a dump.
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
  (`logged_out`) notification to management; link state (`disconnected` /
  `connected`) too, with `LINK_EVENTS`

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
  digits as sender_address (rare; the mapping fills in as messages flow).

Also working: group subjects → conversation names (on first sight and on
renames), history sync import (messages + pushnames, chunked), LID → phone
canonicalization for contact addresses.

## Development

```bash
go build ./...   # or: docker build .
```
