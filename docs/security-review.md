# Security reviews

## 0.3.1

`cloudflare_jwt` was removed. `/mcp` again authenticates with the bearer tokens, or with nothing when `auth_method` is `none`.

## 0.2.0

Scope: the 0.2.0 hardening pull request (#2). Local proofs only (unit tests with `-race`, a local binary with
`WA_MCP_NO_CONNECT=1`, CI container smoke test); no WhatsApp account was paired.

**Verdict: no Critical or High findings.** A Medium from code review (data race on token rotation) was fixed
before release.

### Threat model changes

- **Read-only by default.** `enable_send` defaults to `false`. With it off, no token sees the send tools.
- **Separate send token.** `auth_token` is read-only. `send_token` is the only token that lists and calls send
  tools (when sending is on). It must differ from `auth_token` and meet the same strength rules. The tool set is
  chosen per request by token; send handlers also re-check the token.
- **Allowlist required.** An empty `allowed_recipients` refuses every send (in 0.1.0 empty meant anyone).
- **Data at rest.** Data folder `0700`; session, message and token files `0600`, re-applied on every start;
  downloaded media `0600` in `0700` folders.
- **Logs.** Other people's JIDs and phone numbers masked in every line; no client address in failed-login lines;
  the pairing QR is not written to the add-on log.

### Checks and results

| Area | Result |
|---|---|
| Token roles | Both token hashes compared in constant time on every request. Read token: read tools only; calling a send tool is a tool error (not a 401), so it never feeds the per-caller failed-login limiter (45 calls, no lockout; test). Send token while `enable_send` is off: 403, not counted as a failed login (test). |
| Token rules | `send_token` equal to `auth_token`, short or predictable: configuration error (tests). A stored generated token equal to the other token is replaced. |
| Recipients | Empty `allowed_recipients`: every send refused with a clear error (test). Config errors no longer repeat the entry (it may be a phone number). |
| Received file names | Base name only, `[A-Za-z0-9._-]` allowlist, `..` broken up, no leading dot; `os.Root` confinement; exclusive create, never overwrites (`-2`, `-3`, ...); planted symlinks not followed. Tests cover `../../x`, absolute paths, backslashes, NUL/control characters, RTL override, fullwidth solidus, two-dot leader. |
| Permissions | Start-up tightening through `os.Root`, skips symlinks, leaves `options.json` alone (test; CI asserts `0700`/`0600`). |
| Log masking | JIDs, 8+ digit numbers and grouped numbers with 9+ digits (`+351 900 000 000`, `351-900-000-000`) masked in messages and string, error and Stringer attributes; dates, times and IP addresses kept (tests). The masking handler is installed before the configuration is read. |
| Concurrency | Whether sending is active is fixed at start-up; page rotation no longer writes configuration fields (live tokens only change inside the authenticator). `-race` test rotates both tokens while `send_message` and `get_status` run. |
| Supply chain | `govulncheck` clean; Dependabot for Go modules and GitHub Actions. |

### Lows and how they were resolved

| Finding | Resolution |
|---|---|
| L1 Data race between page rotation and `SendActive()` | Immutable send state; rotation keeps current tokens under the page lock; `TestRotateWhileSendingNoRace`. |
| L2 Generated send token can be revealed or rotated from the shared ingress origin | Documented: set `send_token` in the configuration to disable page reveal and rotation. |
| L3 Configuration error could print an `allowed_recipients` entry before masking | Masking handler installed first; the error names the entry number only. |
| L4 Grouped phone numbers not masked | Grouped-number masking with a 9-digit threshold. |
| I1 Pairing QR in the add-on log | Printed only when no pairing page exists (plain Docker); the add-on shows it only on the admin-only page. |
| Send token while sending is off counted as failed login | 403 without recording a failure. |

### Remaining risks

- **Outer layer.** The endpoint should only be reachable through a tunnel protected by Cloudflare Access (or
  similar) in addition to the bearer token; they are separate layers and both are needed.
- **Prompt injection on the send client.** A client holding the send token reads untrusted messages and can send.
  Limited by the allowlist, rate limits and outbox confinement, not eliminated.
- **Pairing QR in plain Docker.** Without a page it is still printed in the container log.
- **Unofficial client.** WhatsApp may restrict the number.
- Still open from 0.1.0: Low 5 (Node 20 action pins), Low 6 (go-qrcode).

## 0.1.0

Scope: everything in this repository at the 0.1.0 pull request (Go server, add-on config, Dockerfile, CI).
Reviewer stance: adversarial, local proofs only (unit tests and a local binary with `WA_MCP_NO_CONNECT=1`);
no WhatsApp account was paired and no third-party system was probed.

**Verdict: no Critical or High findings. Release not blocked.** Mediums are accepted and documented below.

### Threat model

| Asset | Impact if lost |
|---|---|
| whatsmeow session keys (`/data/whatsmeow.db`) | full impersonation of the WhatsApp account |
| message history (`/data/messages.db`) | privacy of all chats |
| bearer token | read all chats, send as the user |
| ability to send | spam, harassment, account ban |

Entry points: `/mcp` from the internet through a tunnel; the hassio Docker network (any other add-on); the
ingress page (Home Assistant administrators); incoming WhatsApp messages from anyone (content reaches agents);
files under `/share` (writable by other add-ons with `share:rw`); CI (pull requests, published images).

### Checks and results

| Area | Result |
|---|---|
| Authentication | Every `/mcp` method needs `Authorization: Bearer`. SHA-256 of both sides compared with `subtle.ConstantTimeCompare`. Configured tokens ≥ 32 printable chars, ≥ 10 distinct characters and not a short repeated pattern; generated tokens 256 bits, stored 0600, never logged (CI asserts the token is absent from the container log). |
| Routes | Exposed port: `/mcp` and `GET /healthz` (`{"status":"ok"}` only). Everything else 404; `//mcp`, `/./mcp` redirect, `/mcp/`, `/MCP`, encoded variants 404 (verified locally). No bridge REST API exists. |
| Ingress page | Separate port, not published; only `172.30.32.2` (Supervisor ingress proxy) may connect, so other add-ons get 403; `panel_admin: true`; POSTs need `X-Requested-With` and are refused for `Sec-Fetch-Site: cross-site`; CSP `default-src 'none'`, no inline script, DOM writes via `textContent`. All ingress pages share the Home Assistant origin, so another add-on's page could call these endpoints with the custom header; therefore a generated token is revealed only once (POST, persisted marker) and can afterwards only be rotated. |
| CSRF / browsers | `/mcp` wrapped in `http.CrossOriginProtection` (cross-site browser POST → 403, verified). The MCP SDK's DNS-rebinding protection only applies to requests that arrive on a loopback address, which this server never receives in the add-on (it listens on the container network), so it is not relied on; the bearer token is what stops a rebinding page. |
| Injection | All SQL parameterised; `LIKE` wildcards escaped; recipients validated (digits or user/LID/group JID; broadcast, status, newsletter, bot refused). |
| File read (send) | `send_file`/`send_audio_message` read through `os.Root` on `/share/whatsapp-mcp/outbox`: `..`, absolute paths elsewhere, symlinks out of the folder, a symlinked outbox and an outbox behind a symlinked parent are refused (tests). The root is opened first and its device/inode compared with `Lstat` of the symlink-free path. Size cap `max_media_mb`. Recipients can be restricted with `allowed_recipients`. |
| File write (download) | Off by default. Writes through `os.Root` into `/share/whatsapp-mcp/downloads` with sanitised names; planted symlinks are not followed (test). The whatsmeow media HTTP client refuses responses announcing more than `max_media_mb` + 64 KiB and aborts bodies that grow past it (test), so a sender-controlled size cannot exhaust memory. AppArmor confines all server writes to `/data` and `/share/whatsapp-mcp` (enforced and tested in CI). |
| Rate limits / DoS | Sends: token bucket per minute + rolling 24 h cap (default 50), global. Request body 1 MiB (413 verified), header timeouts, list limits and `page` (≤ 1000) capped, stateless MCP (no session table to exhaust). Failed auth throttled per client (10, then 1 per 6 s); the table evicts the least recently seen entry when full; failed-auth log lines are globally rate limited. |
| Logging | Message contents, captions and tokens are never logged; whatsmeow debug logging is never enabled (debug add-on level maps whatsmeow to INFO). |
| Data at rest | `/data` 0700, databases and token 0600 (umask 077; asserted in CI). Optional `retention_days` deletes old messages and vacuums incrementally. |
| Supply chain | Go modules verified (`go mod verify` in the image build), `govulncheck` in CI (found and fixed GO-2026-6859 by moving to Go 1.27.4), Go and Home Assistant base images pinned by `@sha256` digest, actions pinned by commit SHA, workflow `contents: read` by default, `packages: write` only for the build job, publishing only on push to main/tags and only when `PUBLISH_IMAGES` is set, no `pull_request_target`. |

### Findings

| # | Severity | Finding | Status |
|---|---|---|---|
| 1 | Medium | **Prompt injection to send.** Any contact can put instructions in a message; an agent with send tools may follow them and message arbitrary numbers or forward outbox files. | 0.1.0: mitigated (send on by default at the owner's request): optional `allowed_recipients`, daily cap 50, `untrusted` object, server instructions, README warning; residual risk when the allowlist was empty. Since 0.2.0: send off by default, separate send token, empty allowlist refuses every send (see below). |
| 2 | Medium | **Single shared token.** All clients share one token; no per-client revocation or audit. | 0.1.0: accepted. Since 0.2.0: read-only `auth_token` and a separate `send_token`. |
| 3 | Low | Failed-auth throttling is keyed by remote address + `CF-Connecting-IP`, which a caller can vary. It does not stop a determined guesser; token entropy does. | Documented; full reset replaced by evict-oldest, logs rate limited (round 2). |
| 4 | Low | Base images referenced by tag, not digest. | Fixed: pinned by `@sha256` (round 2). |
| 5 | Low | `actions/checkout` v4, `docker/*` v3/v6 pins run on the deprecated Node 20 runtime (CI warning). | Open: bump pins in a later PR. |
| 6 | Low | `skip2/go-qrcode` (QR PNG for the page) has had no release since 2020. Input is only the pairing code from whatsmeow. | Accepted. |
| 7 | Info | Container runs as root (Home Assistant default). The token and session are in Home Assistant backups. | Accepted. |
| 8 | Info | Connecting an unofficial client may violate WhatsApp's terms and risk a ban. | README warns. |

Re-verification after fixes: `go test -race ./...` green, `govulncheck` clean, CI build and container smoke test
green on aarch64 and amd64.

### Independent review (round 2, at 64ea993)

An independent reviewer found no Critical or High issues. Findings and resolution:

| Finding | Severity | Resolution |
|---|---|---|
| Media download OOM: whatsmeow reads media with an unbounded `io.ReadAll`, and the advertised file length comes from the sender | Medium | `SetMediaHTTPClient` with a transport that refuses `Content-Length` > `max_media_mb` + 64 KiB and wraps bodies in a reader that errors on overflow; uploads (POST) unaffected. `TestMediaHTTPClientLimits`. |
| Prompt-injection sends | Medium | 0.1.0: `allowed_recipients` (empty = anyone, recommended; since 0.2.0 empty refuses every send), daily cap 50, `untrusted` object for other people's text, README warning. `TestAllowedRecipientsEnforced`, `TestReadTools`. |
| Writes not confined | Low | `apparmor.txt`: the server runs in a child profile that can only write to `/data`, `/share/whatsapp-mcp`, `/tmp`. CI loads the profile, checks the server's label and that a write to `/opt` fails. |
| No message retention | Low | `retention_days` (default 0 = keep): periodic `DELETE` + `PRAGMA incremental_vacuum` (`auto_vacuum=INCREMENTAL`). `TestPruneRetention`. |
| Failed-auth log flood; full reset of the throttle table | Low | Global log rate limit with a suppressed counter; evict least recently seen entry. `TestAuthEvictsOldestAndLimitsLogs`. |
| Check-then-open race on media folders | Low | Open the `os.Root` first, then compare its `Stat` with `Lstat` of the symlink-free path (`os.SameFile`, device + inode). `TestOutboxBehindSymlinkedParentRefused`. |
| Shared ingress origin can read the generated token | Low | Token shown once, rotation endpoint, documented in README and on the page; every reveal and rotation is logged (without the token) so an unexpected one is visible. Residual: rotation also returns the new token, so a malicious ingress page opened by an admin could still obtain one; setting `auth_token` removes this. `TestPageIngressOnly`. |
| No watchdog; halt on every exit | Low | `watchdog: http://[HOST]:[PORT:8120]/healthz`; exit code 2 (configuration error) stops the add-on, other exits restart after 5 s. CI checks exit code 2. |
| Inaccurate DNS-rebinding claim; images not pinned by digest | Low | Text corrected above; Go and Home Assistant base images pinned by `@sha256`. |
| Unbounded `page`; weak configured tokens | Low | `page` capped at 1000; tokens need ≥ 10 distinct characters and no short repeated pattern. `TestPageCapped`, `TestWeakTokensRejected`. |

Re-check after the fixes (7e91b00 and later): no Critical or High findings; AppArmor confinement observed enforcing
in CI (`mkdir /opt/evil: permission denied`).

Not changed: the ghcr image publication (no `image:` in `config.yaml`, publishing gated by `PUBLISH_IMAGES`), as
requested. Still open: Low 5 (Node 20 action pins) and Low 6 (go-qrcode), see the table above.
