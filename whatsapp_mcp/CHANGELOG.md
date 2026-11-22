# Changelog

## 0.3.1

- Removed `cloudflare_jwt` and the `cf_team_domain` / `cf_access_aud` options. Authentication is `api_key` (default) or `none`. An installation that still has those options saved must clear them before updating.

## 0.3.0

- **Authentication method.** `auth_method` chooses how `/mcp` authenticates callers:
  - `api_key` (default, unchanged): `auth_token` for the read tools and `send_token` for the send tools.
  - `cloudflare_jwt`: a Cloudflare Access application JWT. Accepted from `Cf-Access-Jwt-Assertion`, from `Cf-Access-Token` (an MCP portal or another server forwarding that JWT), from the `CF_Authorization` cookie, or from `Authorization: Bearer` when the bearer value is that same JWT. Any other bearer is rejected. Requires `cf_team_domain` and `cf_access_aud`. Signing keys are loaded only from `https://<team>.cloudflareaccess.com/cdn-cgi/access/certs` (RS256). With `enable_send` on, every identity the Access policy allows can send.
  - `none`: no authentication. Not recommended. The add-on logs a warning on every start. With `enable_send` on, every caller can send.
- Existing configurations without `auth_method` stay on `api_key`.

## 0.2.0

Security hardening. **Read before updating** if you used the send tools in 0.1.0.

- **Two tokens.** `auth_token` is now the read-only token: clients using it see only the read tools, exactly as
  before for reading. The new `send_token` (password field; generated and shown once on the add-on page when
  empty) is the only token that can list and call `send_message`, `send_file` and `send_audio_message`. It must
  differ from `auth_token` and follows the same strength rules. Calling a send tool with the read-only token is a
  tool error, not an authentication failure, so it never counts against the failed-login limit.
- **Send tools off by default.** The `enable_send` default changed from `true` to `false`. When it is off, no token
  sees the send tools and the send token is not accepted. An existing installation keeps its saved
  `enable_send: true`, but sending still needs the send token.
- **`allowed_recipients` empty now refuses every send** (in 0.1.0, empty meant anyone). List the numbers or JIDs
  agents may write to. The existing `send_rate_per_minute` and `send_daily_limit` still apply.
- **Received file names.** `download_media` already kept only a sanitised base name inside the downloads folder
  (through `os.Root`); it now also breaks up `..` sequences, never overwrites an existing file (exclusive create,
  `-2`, `-3`, ... suffix) and writes media `0600` in `0700` folders (was `0640`/`0750`).
- **Permissions.** On every start the data folder is set to `0700` and the session, message and token files to
  `0600`, and downloaded media to `0600` in `0700` folders, including files left by older versions. Symlinks are
  skipped.
- **Logs.** Other people's phone numbers and JIDs are masked in every log line, including whatsmeow warnings and
  errors. Failed-login lines no longer include the client address.
- Add-on page: shows the generated send token once (with rotation) and warns when sending is on but
  `allowed_recipients` is empty.
- The send token while `enable_send` is off is refused with 403 and does not count as a failed login.
- The pairing QR code is no longer written to the add-on log; scan it on the add-on page (plain Docker without a
  page still prints it). Grouped phone numbers (`+351 900 000 000`) are masked too, and configuration errors no
  longer repeat an `allowed_recipients` entry.
- Fixed a data race between token rotation on the page and send requests.
- Dependabot for Go modules and GitHub Actions.

## 0.1.0

- First release: personal WhatsApp account linked as a device (whatsmeow), MCP over streamable HTTP on port 8120
  at `/mcp`, bearer token required (configured or generated), pairing QR code in the log and on an ingress page,
  re-pairing after an unlink.
- Tools: get_status, search_contacts, list_chats, get_chat, get_direct_chat_by_contact, get_contact_chats,
  list_messages, get_message_context, get_last_interaction, send_message, send_file, send_audio_message,
  download_media (off by default).
- Text written by other people (message text, captions, file names, contact, chat and group names) is returned in
  an `untrusted` object.
- Sending: `allowed_recipients` (optional, recommended), rate limits (6 per minute, 50 per 24 hours by default),
  files only from the outbox folder.
- Media downloads from WhatsApp's servers are capped at `max_media_mb` while streaming.
- `retention_days` (default 0 = keep) deletes old messages every 6 hours and frees the space.
- Generated token shown once on the add-on page, with token rotation; weak configured tokens are refused.
- AppArmor profile: the server may only write to `/data` and `/share/whatsapp-mcp`.
- Watchdog on `/healthz`; an invalid configuration stops the add-on instead of restarting it in a loop.
- Base images pinned by digest.
