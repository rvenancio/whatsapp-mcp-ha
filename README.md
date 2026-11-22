# WhatsApp MCP add-on for Home Assistant

A Home Assistant add-on that links **your personal WhatsApp account as a device** (the same mechanism as WhatsApp
Web, through [whatsmeow](https://github.com/tulir/whatsmeow)) and exposes it as an **MCP server over streamable
HTTP**, so AI agents can search and read your chats and, if you allow it, send messages and files.

- Personal use only. It does **not** use the WhatsApp Business API. Unofficial clients are against WhatsApp's
  terms of service and can get a number restricted; use it at your own risk and keep sending volumes low.
- One Go binary: the whatsmeow client, the local message store and the MCP server. There is no separate bridge
  REST API, so nothing besides `/mcp` and `/healthz` listens on the exposed port.
- Session and messages are stored in the add-on's `/data` (included in Home Assistant backups).

Independent project, not affiliated with WhatsApp or Meta. The tool set and message store are derived from
[lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp) (MIT).

## Install (Home Assistant OS, aarch64 or amd64)

1. **Settings → Add-ons → Add-on store → ⋮ → Repositories**, add `https://github.com/rvenancio/whatsapp-mcp-ha` exactly, with no `.git` and no trailing slash.
2. Install **WhatsApp MCP**. The first install builds the image on the device and takes a few minutes.
3. In **Configuration**, leave **Authentication** on `api_key` (the default) and, optionally, set `auth_token`, the
   read-only token (32+ random printable characters, e.g. `openssl rand -base64 32`). If you leave it empty, the
   add-on generates one and shows it **once** on its page. `none` turns authentication off and is not recommended. Sending is off by default; see [Sending](#sending).
4. **Start** the add-on and open **Open Web UI** (or the sidebar entry **WhatsApp MCP**, administrators only).
5. On your phone: **WhatsApp → Settings → Linked devices → Link a device**, and scan the QR code shown on the page.
   The page shows **Connected** when done. In the add-on the QR code is only on the page, never in the log.

A watchdog restarts the add-on if the endpoint stops answering. An invalid configuration stops the add-on with
the error in the log instead of restarting it.

If the QR codes expire, press **Show a new QR code** on the page (or restart the add-on). If WhatsApp unlinks the
device (from the phone, or after a long time offline), the add-on goes back to showing a QR code. **Unlink this
device** on the page logs out and starts a new pairing.

## Connect an MCP client

| | |
|---|---|
| Transport | streamable HTTP (stateless, JSON responses) |
| Path | `/mcp` |
| Port | `8120` (container port) |
| From other add-ons (e.g. cloudflared) | Internal URL on the add-on page. For this repository URL it is `http://910e8e2f-whatsapp-mcp:8120/mcp` |
| Header | depends on `auth_method` (below) |

The name is not fixed. Home Assistant takes the first 8 characters of the SHA-1 of the repository URL **exactly as you added it**, then appends `-whatsapp-mcp`. The URL above, with no `.git` and no trailing slash, hashes to `910e8e2f`. Adding `.git` produces a different name. Copy **Internal URL** from the add-on page (**Open Web UI**). Other add-ons can resolve it; a browser cannot.

The port is **not published on the host** by default. To use it from the LAN, set a host port under
**Configuration → Network**. To use it from outside, put a tunnel in front (e.g. a Cloudflare Tunnel with the
internal URL above as the service). Do not forward the port on your router.

### `api_key` (default)

`Authorization: Bearer <token>` — `auth_token` for the read tools, `send_token` for the read and send tools.
Cloudflare Access, if you also put it in front, is a separate layer: Access decides who reaches the hostname, the
bearer token decides what the caller may do. Keep both if you expose the hostname.

```json
{ "mcpServers": { "whatsapp": { "type": "http", "url": "https://<your-hostname>/mcp",
  "headers": { "Authorization": "Bearer <token>" } } } }
```

### `none` (not recommended)

`/mcp` accepts every caller. Use it only when port 8120 cannot be reached by anything you do not trust (it is not
published on the host unless you set a host port). The log warns on every start. With `enable_send` on, every
caller can send.

`GET /healthz` answers `{"status":"ok"}` without authentication and without any other detail.

## Tools

| Tool | |
|---|---|
| `get_status` | connection state, limits, outbox folder |
| `search_contacts` | by name or number (address book synced from the phone, plus known chats) |
| `list_chats`, `get_chat`, `get_direct_chat_by_contact`, `get_contact_chats` | chats |
| `list_messages`, `get_message_context`, `get_last_interaction` | messages (search, filters, context) |
| `send_message` | send token only: text to a phone number (country code, digits only) or a JID (`…@s.whatsapp.net`, `…@lid`, group `…@g.us`) |
| `send_file` | send token only: image, video, audio or document **from the outbox folder only** |
| `send_audio_message` | send token only: an Ogg Opus `.ogg` file from the outbox as a voice note |
| `download_media` | only when `allow_media_download` is on; saves into `/share/whatsapp-mcp/downloads` |

Message text, captions, file names and contact, chat and group names are returned inside an `untrusted` object
in every result, so agents can tell other people's words from data they may act on.

Only messages received while the add-on runs, plus what WhatsApp sends in the history sync after pairing, are
searchable.

Limitations: edits, deletions ("deleted for everyone") and reactions are not applied to stored messages;
voice notes must already be Ogg Opus (no ffmpeg in the image); passkey pairing is not supported (use the QR code).

## Configuration

| Option | Default | |
|---|---|---|
| `auth_method` | `api_key` | `api_key` (bearer tokens) or `none` (not recommended) |
| `auth_token` | empty | read-only bearer token, used only when `auth_method` is `api_key`; empty = generated, shown once on the add-on page |
| `send_token` | empty | only for `api_key`: the only token that gets the send tools (when `enable_send` is on); must differ from `auth_token`; empty = generated, shown once on the add-on page |
| `enable_send` | `false` | `true` adds the send tools. With `api_key` only `send_token` can call them. With `none`, every caller can |
| `send_rate_per_minute` | `6` | sends per minute, all clients together |
| `send_daily_limit` | `50` | sends per rolling 24 hours |
| `allowed_recipients` | empty (no sends) | phone numbers (country code, digits) or JIDs the send tools may write to |
| `allow_media_download` | `false` | enables `download_media` |
| `max_media_mb` | `16` | largest file sent or downloaded (also caps media downloads from WhatsApp's servers) |
| `retention_days` | `0` | delete stored messages older than this; `0` keeps everything |
| `log_level` | `info` | message contents and tokens are never logged; other people's numbers and JIDs are masked |

## Sending

1. Set `enable_send: true`.
2. Set `allowed_recipients`, e.g. `+351900000000` or `120363000000000000@g.us`. Empty refuses every send.
3. Set `send_token` (32+ random characters, different from `auth_token`), or leave it empty and copy the generated
   one from the add-on page (shown once; **Generate a new send token** replaces it). A `send_token` set in the
   configuration is never shown or replaced from the page, which is the safer choice.
4. Give the send token **only** to the one client that should send. Never put it into a shared or portal client;
   those keep using `auth_token`, which never sees the send tools.

`allowed_recipients` is compared with the recipient exactly as the agent passes it (normalised to a JID): a phone
number allows `…@s.whatsapp.net`; if a contact is addressed by its `…@lid` JID, list that form as well.

**Sending files:** copy them to `/share/whatsapp-mcp/outbox/` (e.g. with the Samba or File editor add-on) and pass
the file name as `media_path`. Paths outside that folder, `..` and symlinks pointing out of it are refused.

## Plain Docker (e.g. a NAS)

```sh
git clone https://github.com/rvenancio/whatsapp-mcp-ha && cd whatsapp-mcp-ha
docker build -t whatsapp-mcp --build-arg BUILD_FROM=ghcr.io/home-assistant/amd64-base:3.24 whatsapp_mcp
docker run -d --name whatsapp-mcp --restart unless-stopped \
  -p 8120:8120 \
  -v "$PWD/data:/data" -v "$PWD/media:/share/whatsapp-mcp" \
  -e WA_MCP_TOKEN="<32+ random characters>" \
  whatsapp-mcp
docker logs -f whatsapp-mcp   # no pairing page in plain Docker: scan the QR code printed here
```

Use `aarch64-base` on ARM. Without `WA_MCP_TOKEN` a token is generated into `data/auth_token`. Other variables:
`WA_MCP_ENABLE_SEND`, `WA_MCP_SEND_TOKEN` (generated into `data/send_token` when sending is on and it is unset), `WA_MCP_SEND_RATE_PER_MINUTE`, `WA_MCP_SEND_DAILY_LIMIT`, `WA_MCP_ALLOW_MEDIA_DOWNLOAD`,
`WA_MCP_MAX_MEDIA_MB`, `WA_MCP_LOG_LEVEL`, `WA_MCP_ALLOWED_RECIPIENTS` (comma-separated), `WA_MCP_RETENTION_DAYS`,
`WA_MCP_AUTH_METHOD` (`api_key` or `none`). The pairing page is off in plain Docker (it has no login of its own).
To re-pair after an unlink, run `docker restart whatsapp-mcp` and scan the new code from the log.

## Security

See [SECURITY.md](SECURITY.md) and [docs/security-review.md](docs/security-review.md). In short: `/mcp` requires an
API key unless you turn authentication off, read-only unless you enable
sending, send tools only for the send token,
sends limited to `allowed_recipients` and rate limited, file sending confined to the outbox,
received file names sanitised and never overwriting files, media download off by default, no other routes on the
exposed port, pairing page only through Home Assistant ingress for administrators, message contents and tokens never
logged, other people's numbers masked in logs, data folder `0700` and session, message, token and media files `0600`.

**Warning: prompt injection.** Anyone who can message you can write text that tries to make an agent send
messages or files on your behalf. An agent that can read your messages and also send is the risky combination.
Keep sending off unless you need it; if you enable it, keep `allowed_recipients` short, keep the send limits low,
keep only files you are happy to share in the outbox, and prefer agents that ask you before sending.

**Unofficial client.** whatsmeow is not an official WhatsApp client. WhatsApp can restrict or ban numbers that use
it; a secondary number reduces the impact.

**Rotating tokens.** Changing `auth_token` (or generating a new one on the page) means updating every client that
stores it, e.g. MCP portals or connectors. The same applies to `send_token` for the client that sends.

**Generated token and other add-ons.** All Home Assistant ingress pages share one browser origin, so a page of
another add-on that you open could call this add-on's page too. That is why a generated token is shown only once
(and can only be replaced afterwards). Setting your own `auth_token` avoids showing it at all.

Writes of the server process are confined by an AppArmor profile to `/data` and `/share/whatsapp-mcp`.

## License

MIT, see [LICENSE](LICENSE). whatsmeow is MPL-2.0 (used unmodified as a Go module).
