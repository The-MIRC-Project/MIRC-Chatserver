# MIRC Chat Server

Self-hosted chat server for **MIRC (Modern Internet Relay Chat)**, written in Go.

Like IRC, anyone can run a MIRC chat server on their own IP or domain. Users don't register on your server. They log in with their account from a **MIRC session server**, which gives them the same name, avatar and cosmetics everywhere. Your server only checks with the session server that each user is who they claim to be.

- Single static binary with no external dependencies, plus a Docker image
- TCP transport with one JSON object per line, plus built-in TLS
- Generates a self-signed certificate on first start, or uses your own (e.g. Let's Encrypt)
- Channels with owners, operators, kick, ban and topics
- Persistent channel membership and offline DMs, similar to Discord
- Message history, edit and delete, typing indicators and presence
- Flood protection and size limits
- Data stored as plain JSON files, no database needed

---

## How authentication works

```
Client ── login ──────────────────────▶ Session server   (once, gets a login token)
Client ── TCP/TLS ────────────────────▶ Chat server      ◀── you are here
Client ◀───────────────────── hello ── Chat server
Client ── "token for host:port" ──────▶ Session server   → one-time server token (60 s)
Client ── {"op":"auth","token":…} ────▶ Chat server
                                        Chat server ── POST /api/session/verify ──▶ Session server
                                        Chat server ◀── user profile (200) or 403 ──
Client ◀─────────────────── welcome ── Chat server
```

- Your server **never sees passwords or login tokens**. It only gets a single-use token that the session server issued **for one of your server's addresses**.
- When verifying, the server sends its own list of public addresses (`MIRC_ADDRESSES`). A token issued for another server is rejected. Because of this, a malicious server can't replay a user's token against yours, and you can't replay one against anyone else.
- Each token works exactly once and expires after 60 seconds.

**This means `MIRC_ADDRESSES` must list every `host:port` your users connect with.** If it doesn't, they get *"Token was issued for a different server address"* when they log in.

---

## Quick start

### Docker (chat server only)

Use this when you run a chat server against someone else's session server:

```bash
docker build -t mirc-chatserver .
docker run -d --name mirc-chat --restart unless-stopped \
  -p 6697:6697 \
  -v mirc-chat-data:/data \
  -e MIRC_NAME="My Server" \
  -e MIRC_SESSION_URL="https://accounts.example.org" \
  -e MIRC_ADDRESSES="chat.example.org:6697,203.0.113.5:6697" \
  -e MIRC_ADMINS="yourname" \
  -e MIRC_LISTEN="" \
  mirc-chatserver
```

`MIRC_LISTEN=""` disables the plaintext port, which is what you want on the internet.

### Binary

```bash
go build -o mirc-chatserver .
./mirc-chatserver -name "My Server" -session http://localhost:8440 -admins yourname
```

Prebuilt binaries for Linux (amd64/arm64) and Windows are produced by `../build-servers.sh` into `../bin/`.

### First connection

When the server starts, it logs its certificate fingerprint:

```
TLS-Fingerprint (SHA-256): 38:05:BE:6C:…:50:A4
```

The MIRC client shows this fingerprint the first time someone connects, so share it with your users so they can check it. With a certificate from a public CA, the client connects without asking.

---

## Configuration

Every option can be set as a command-line flag or as an environment variable. The environment variable is the default, and a flag overrides it.

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `-listen` | `MIRC_LISTEN` | `:6680` | Plaintext listener. Empty disables it. |
| `-tls-listen` | `MIRC_TLS_LISTEN` | `:6697` | TLS listener. Empty disables it. |
| `-tls-cert` | `MIRC_TLS_CERT` | *(empty)* | PEM certificate (full chain). Empty generates a self-signed one in `<data>/tls/`. |
| `-tls-key` | `MIRC_TLS_KEY` | *(empty)* | PEM private key |
| `-name` | `MIRC_NAME` | `My MIRC Server` | Server name shown in clients |
| `-motd` | `MIRC_MOTD` | `Welcome to this MIRC server!` | Message of the day |
| `-session` | `MIRC_SESSION_URL` | `http://localhost:8440` | URL the server uses to **verify tokens** (can be an internal address) |
| `-public-session` | `MIRC_PUBLIC_SESSION_URL` | *(= `-session`)* | Session server URL **announced to clients**. It must exactly match the URL users log in with. |
| `-addresses` | `MIRC_ADDRESSES` | *(empty)* | Comma-separated public `host:port` list (see above). If empty, only `localhost`/`127.0.0.1` on the configured ports is accepted, which is fine for testing only. |
| `-channels` | `MIRC_CHANNELS` | `#general,#offtopic` | Default channels. They are created at startup and new users join them automatically. |
| `-admins` | `MIRC_ADMINS` | *(empty)* | Comma-separated usernames with operator rights in every channel |
| `-data` | `MIRC_DATA` | `./data` (`/data` in Docker) | Data directory |

### Why there are two session URLs

In Docker, the chat server reaches the session server internally, e.g. `http://session:8440`. Clients reach it through its public URL, e.g. `https://accounts.example.org`.

- `MIRC_SESSION_URL` is the internal URL, used for token verification.
- `MIRC_PUBLIC_SESSION_URL` is the public URL, sent to clients in `hello`.

Clients refuse to connect if the public URL doesn't match the session server they're logged in to. This stops a server from tricking users into using a different account system.

### Using your own certificate

```bash
-e MIRC_TLS_CERT=/certs/fullchain.pem \
-e MIRC_TLS_KEY=/certs/privkey.pem \
-v /etc/letsencrypt/live/chat.example.org:/certs:ro
```

Restart the server after renewing the certificate. Note that Let's Encrypt live files are symlinks, so mount the whole `/etc/letsencrypt` directory or copy the files.

---

## Running on the internet

1. Set `MIRC_ADDRESSES` to **every** hostname and IP:port users connect with, e.g. `chat.example.org:6697,203.0.113.5:6697`.
2. Set `MIRC_PUBLIC_SESSION_URL` to the exact public URL of your session server (HTTPS).
3. Disable plaintext with `MIRC_LISTEN=""` and expose only port 6697.
4. Open TCP port **6697** in your firewall and router.
5. Optionally, use a real certificate so users aren't asked to trust a fingerprint.

The chat server needs outbound HTTP(S) access to the session server.

> **Reverse proxies:** MIRC uses raw TCP, not HTTP. A normal HTTP reverse proxy can't front it. Use a TCP/stream proxy (e.g. nginx `stream {}`, HAProxy in TCP mode, Traefik TCP routers), or expose the port directly.

The server has no HTTP endpoint. For health checks, use a TCP connect to port 6697, as the Docker `HEALTHCHECK` does.

---

## Roles and moderation

| Role | How you get it | Can |
|---|---|---|
| **Owner** | Create the channel (`/join #new`) | Everything an op can do, plus `/op`, `/deop` and `/delchannel` |
| **Operator** | Granted by the owner, or listed in `MIRC_ADMINS` | `/topic`, `/kick`, `/ban`, `/unban`, delete any message in the channel |
| **Server admin** | Listed in `MIRC_ADMINS` | Operator in every channel, can grant ops and delete any channel |
| **Member** | Join a channel | Chat, edit and delete own messages |

Ops can't kick the channel owner. Only the owner or an admin can kick other ops, and only admins can kick admins. Bans are per channel and per account.

There are no server-wide bans yet. To lock someone out completely, delete or disable their account on the session server.

---

## Data

```
data/
├── state.json           channels (topic, owner, ops, bans, members), known profiles, DM partners
├── history/*.jsonl      one append-only log per channel / DM conversation
└── tls/
    ├── cert.pem         generated certificate (if no own cert is configured)
    └── key.pem
```

- **Backups:** copy the whole data directory, ideally while the server is stopped.
- History files are append-only JSON Lines. Edits and deletions are appended as separate records and applied on load. The last 1000 messages per conversation are kept in memory, and the full log stays on disk.
- **Keep `tls/key.pem` when migrating**, otherwise users see a "certificate changed" warning.
- DMs are stored in plaintext on your server. As the operator, you are responsible for this data, so tell your users.

---

## Limits

| Limit | Value |
|---|---|
| Line (frame) size | 64 KB |
| Message length | 2000 characters |
| Topic length | 300 characters |
| Flood limit | 6 messages per 4 seconds per connection |
| Channels per server | 500 |
| History page size | 50 by default, 100 max |
| Login timeout | 30 seconds after connecting |
| Idle timeout | 90 seconds without any incoming line (the server pings every 30 s) |
| Send queue | 256 messages. Slower clients are disconnected. |

Channel names are `#` followed by 1–32 letters, digits, `_`, `-` or German umlauts.

---

## Protocol at a glance

Plain TCP or TLS, UTF-8, one JSON object per line terminated by `\n`.

```text
$ openssl s_client -quiet -connect localhost:6697
{"op":"hello","protocol":"mirc/1","server":{"name":"My Server","motd":"…"},"sessionServer":"http://localhost:8440","tls":true}
{"op":"auth","token":"<64 hex chars from /api/session/server-token>"}
{"op":"welcome","me":{…},"isAdmin":false,"channels":[…],"dms":[…]}
{"op":"msg","target":"#allgemein","text":"Hello!"}
{"op":"message","message":{"id":"…","target":"#allgemein","from":"alice","text":"Hello!","kind":"text","ts":1792…}}
```

**Client → server:** `auth`, `ping`/`pong`, `join`, `part`, `msg`, `edit`, `delete`, `history`, `typing`, `topic`, `list`, `whois`, `open_dm`, `kick`, `ban`, `unban`, `op`, `deop`, `delete_channel`, `refresh_profile`

**Server → client:** `hello`, `welcome`, `channel`, `member_join`, `member_part`, `message`, `message_edit`, `message_delete`, `history`, `dm_open`, `typing`, `topic`, `role`, `presence`, `user_update`, `channel_list`, `whois`, `channel_deleted`, `error`, `ping`, `pong`

Any request may carry a `ref` field. Replies and errors for that request echo it back. The full specification is in [`../PROTOCOL.md`](../PROTOCOL.md).

---

## Troubleshooting

| Log message / symptom | Cause and fix |
|---|---|
| `WARNING: -addresses/MIRC_ADDRESSES not set` | Only local connections can log in. Set `MIRC_ADDRESSES`. |
| `Authentication rejected … token for X, this server only accepts …` | The client connected via address `X`, which isn't in `MIRC_ADDRESSES`. Add it. |
| `Authentication rejected … session server is unreachable` | The chat server can't reach `MIRC_SESSION_URL`. Check the URL, DNS and the Docker network. |
| Client says *"Server uses a different session server"* | `MIRC_PUBLIC_SESSION_URL` doesn't exactly match the URL the user logged in with (scheme, host, port, trailing path). |
| Users get *"Certificate has changed!"* | `tls/key.pem` was regenerated (lost volume, new container without a volume). Restore it from backup, or tell users the new fingerprint. |
| Avatars don't show up in clients | That's a session server problem: its `MIRC_PUBLIC_URL` must be reachable by clients. |

---

## Source layout

| File | Purpose |
|---|---|
| `main.go` | Configuration, listeners, token verification against the session server |
| `conn.go` | Per-connection reader/writer, line framing, auth handshake, keepalive, flood limit |
| `hub.go` | Channels, DMs, roles, moderation, presence and event broadcasting |
| `history.go` | Message IDs, append-only history logs, paging, edits and deletions |
| `tlscert.go` | Self-signed certificate generation and fingerprint logging |
| `common.go` | Shared types (profiles, cosmetics) and atomic JSON file storage |
| `Dockerfile` | Multi-stage build that runs as an unprivileged user with the `/data` volume |

Requires Go 1.24 or newer. It uses only the standard library.

## License

GPL-3.0