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
- **Inbound traffic outlives a failed post** — WhatsApp has the message acked
  before the bridge sees it, so every batch for `whatsapp-web-webhook` is
  written to `bridge_outbox` (same database) first. A lane per session posts
  them oldest first, retrying an outage on a 1 s → 1 min ladder for 12 hours
  and a 500 for 5 minutes; a 4xx is the receiver's final answer and is
  dropped. A restart resumes every lane.
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
| `HISTORY_DAYS`  | no       | How many days of history a pairing asks the phone for, as a full sync. Default `60`. The request rides the pairing, so a number already paired keeps what it got |

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
- The groups, on the session `{address}`; `{group}` is the group's JID, the
  `conversation_address` its messages carry. Members are canonical phone
  digits both ways. Each call answers when WhatsApp has; the group stays
  WhatsApp's, and what the consumer sees of a change afterwards is the
  group's own line about it. WhatsApp's own refusals pass through as 4xx:
  403 when the account is no admin of the group, 404 for a group it is not
  in, 406 for a subject it will not take.
  - `POST /groups/{address}` — `{name, members}` makes a group and answers
    it: `{address, name, members: [{address, name?, admin?}], not_added?}`.
    The group exists even when a seat could not be filled; `not_added`
    names each such person with WhatsApp's code (403: their settings keep
    strangers from adding them, an invite is the way in; 409: already in).
  - `GET /groups/{address}/{group}` — `{address, name, members}`, the group
    as WhatsApp holds it.
  - `PATCH /groups/{address}/{group}` — `{name}` sets the subject.
  - `DELETE /groups/{address}/{group}` — the account leaves.
  - `POST /groups/{address}/{group}/members` — `{members}` seats them; a
    seat the server would not fill fails the call (422) naming each.
  - `DELETE /groups/{address}/{group}/members` — `{members}` takes them
    out; somebody not in the group is a 404 before anything is sent.

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
- Polls and votes in — DataPart kind `poll` (`question`, `options`,
  `selectable_count`, 0 = any number) and `poll_vote` (`question`, `selected`,
  `re_message_id` the poll; each replaces the voter's last, empty =
  withdrawn). A vote reads only on a poll the bridge saw: its choices are
  hashes, matched against the options kept in `bridge_polls`
- A bot's choices in, and presses out — reply buttons, lists, native-flow
  buttons and template quick replies read as DataPart kind `interactive` (the
  body as its text; `form`, `header`, `footer`, `button`, `options` each with
  `id` and `title`, and `links` for URL and call buttons), kept in
  `bridge_interactive`. A text whose `re_message_id` is one of them and that
  names an option by title or id goes out as the response that form expects
  (buttons, list, native-flow or template reply); naming none of its options
  is a 422 that lists them
- A message the bridge cannot read arrives as DataPart kind `unsupported`
  (`fields`, the protobuf fields it carried) instead of being dropped
- Replies (`re_message_id` ↔ quoted message) in/out; edits and revokes in
- Delivery/read receipts in; read receipts + typing indicators out
  (`MarkRead`, `SendChatPresence`)
- Typing in: each chat state the session hears is a `typing` entry of the
  batch (`conversation_address`, `sender_address`, `state` composing or
  paused, `media` audio while recording, `timestamp`), and a log line
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
- WhatsApp Status (stories) is dropped: it is no conversation.
- A channel (newsletter, `<id>@newsletter`) the account owns or administers
  is a conversation at its JID, by the role whatsmeow reads off the channel's
  metadata. Its posts are the account's own, stamped with the session's
  number and an explicit `sent` status, and carry no reactions: followers are
  anonymous. A channel the account only follows is dropped, in live
  traffic, history and the rooms feed.
- A broadcast list is a conversation at its JID, `<id>@broadcast`, live and
  in history. A message somebody sent the account through their list keeps
  the list's address, with the author as `sender_address`; the phone shows it
  in the direct chat with them, and its `muted`/`archived` are that chat's.
- A group's message carries the group's facts: `conversation_members`, how
  many are in it; `conversation_admin`, true when the account is an admin or
  super-admin there; `conversation_community`, the name of the community it
  belongs to. They are read with the group's info, kept, and read again when
  WhatsApp announces a change to the group. Edits carry them too, and a
  group's line on the rooms feed carries them as `members`, `admin` and
  `community`.
- History media is imported as metadata only (old media is frequently gone
  from WhatsApp's CDN): FileParts without a URI render as unavailable
  attachments. History rows always carry explicit final statuses — never
  `pending`, which is OpenBSP's automation gate.
- A LID-only peer — one the store has no phone mapping for — is addressed as
  its full JID, `<lid>@lid`, wherever an address is canonical digits otherwise
  (conversation_address, sender_address, mentions, members, external id
  segments). Bare digits are always a phone number; a dispatch to `<lid>@lid`
  goes to the LID. Once the mapping is known the same person reads as their
  phone number, which is a different address.

Also working: group subjects → conversation names (on first sight and on
renames), history sync import (messages + pushnames, chunked; a pairing asks
for a full sync of `HISTORY_DAYS`), LID → phone canonicalization for contact
addresses.

What a chat says about itself rather than in it rides the batch's `rooms`
feed, a group's or a direct chat's: `{address, name}` alone for the subject a
group is first seen with, and otherwise a line, with `timestamp`, `by` who did
it (canonical digits; absent when WhatsApp itself did, and on the account's own
`view`), and `name`, the room's name now. People are `[{address, name?}]`.

- A group's change, every field WhatsApp announced: `renamed: true` with the
  new `name` and `was`, the subject before, when the bridge knew it;
  `created: true` with the founding roster `joined`; `joined`/`left`
  (`reason: "invite"` for a join by the group's link, with no `by`);
  `promoted`/`demoted`; `description` (`""` deleted); `announce` (only admins
  send), `locked` (only admins edit the info), `disappearing` (seconds, 0 off),
  `approval` (joining needs an admin), `invite_link` (the link, reset);
  `deleted`, `suspended`, `unsuspended`; `linked`/`unlinked:
  [{address, name, type}]` (`parent_group`, `sub_group`, `sibling_group`);
  `picture: "set" | "removed"`; `unknown: [tag]`, a change whatsmeow could not
  read. The account's own arrival is one too: added to a group, it alone
  joined; a group made with it in, created, `by` its creator.
- A direct chat's `disappearing` timer, by whoever set it.
- `call: {state, kind, seconds}`: a call offered to the account is one line
  when it is over, `ended` (seconds from its answer), `missed` or `rejected`;
  a group call is `started` when it is offered.
- `view`, how the account's own app shows the chat, from app state:
  `archived`, `muted` (`until`, RFC 3339, unless it is for always), `pinned`,
  `read`, `cleared`, `deleted`, `blocked` (in the contact's direct chat),
  `locked`, `favorite`, `labels: {added, removed}` (a label's name once an edit
  has named it this process lifetime, else its id). A chat's view at linking
  comes with the history import, one line per chat that has one.

Every line carries the chat's `muted`/`archived` marks, as a message does,
except the one that archives or mutes the chat or lifts it: that line is the
news of the silence.

## Development

```bash
go build ./...   # or: docker build .
```
