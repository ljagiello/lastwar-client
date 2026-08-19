# lastwar-client

A from-scratch Go reimplementation of the *Last War: Survival Game* (`com.fun.lastwar.gp`) mobile
client's network layer: GSL RSA+AES bootstrap crypto, SFS2X wire protocol, and SFSObject binary
codec — enough to log in and drive a real game session over TCP without the Unity client.

Built by reverse-engineering the decompiled APK; see [`docs/`](docs/) for the full protocol
dossier — a [Mintlify](https://mintlify.com) site covering the wire format, crypto, the
~3,178-command catalog, entity IDs, and a running log of what's confirmed live against real
production servers versus what's still static analysis. Start with
[`docs/live-validation.mdx`](docs/live-validation.mdx) for the current confirmed-vs-unconfirmed
picture. Preview the docs locally with `mint dev` from inside `docs/`.

## Status

- **Fully working, live-confirmed:** GSL crypto, SFS2X packet framing (including Zstandard
  decompression), SFSObject codec, brand-new-guest-account login, email-verification account
  binding, reconnecting into an *established* real account's live game state, resource collection
  across 13 confirmed building types (Farmland, Iron Mine, Gold Mine, Smelter, Material Workshop,
  Training Base, Oil Well, Drone Parts Workshop, Component Factory, and the four Season 6 Spore
  Factory tiers — 7 more types are wired in but still unconfirmed, see the building-type table in
  `docs/live-validation.mdx`), and a growing set of account-level automations: the "Armed Truck" idle
  reward, greeting city visitors, bulk-helping alliance members, claiming all mail and alliance
  gifts, donating to the alliance's currently-recommended tech, and both once-a-day VIP claims.
- The earlier "reconnect blocked" / "init push never arrives" problems were never protocol-level
  gates — they were a token-identity mismatch (the client claimed Android while replaying an
  iOS-issued token) and an unimplemented Zstd decoder, respectively. A real server merge later
  exposed a third class of the same kind of bug: `serverInfo` zone-migration redirects were either
  unhandled or unreachable on both login paths. See `docs/live-validation.mdx` for the full
  root-cause writeups.
- **Not yet general-purpose:** the reconnect path currently needs a session config captured from a
  real client login (see below) rather than deriving one from scratch. A from-scratch login using
  `-cs-ios`-equivalent identity from the very first GSL call hasn't been tried yet.

## Scope

Interoperability research: understanding the client-server contract well enough to build a
compatible, from-scratch client. Not in scope: gameplay strategy, economy optimization, or
anything that reads as a cheating/exploit guide rather than protocol documentation — see
[`docs/AGENTS.md`](docs/AGENTS.md) for the full boundary.

## Build

```
go build -o lastwar-client .
go test ./...
```

## Session config (recommended — avoids passing every `-cs-*` flag each run)

Reconnecting into an established account needs several values that can only come from a real
client's own login (device ID, access token, ShuMei fingerprint, ...). Rather than typing them on
the command line every time, put them in a JSON file:

```
cp config.example.json ~/.lastwar_goclient_session.json
chmod 600 ~/.lastwar_goclient_session.json
# then edit it with your own real values
```

`~/.lastwar_goclient_session.json` is auto-loaded on every run if present — no flag needed. To use
a different file, pass `-config /path/to/file.json`. Individual `-cs-*` flags still override
whatever the config file says, for one-off tests.

```json
{
  "ip": "203.0.113.10",
  "port": 17783,
  "zone": "your-real-zone-e.g.-APS1234",
  "gameUid": "your-real-composite-gameUid",
  "deviceId": "your-real-device-id_n3d",
  "shumeiBoxId": "your-real-shumei-fingerprint-token",
  "accessToken": "your-real-access-token-from-a-captured-login",
  "iosMode": true
}
```

**Where these values come from:** capture a real login (e.g. `tcpdump` while the real app logs in,
since the SFS2X game socket is plain TCP with no TLS) and decode the `Login` request — see
`docs/capturing-and-decoding-traffic.mdx` for the exact methodology. `gameUid`/`ip`/`port`/`zone`
also show up in a GSL `getserverlist` response's `serverList[]` entries. The access token is not
single-use, but it *is* bound to the platform identity (`iosMode`) it was issued under, and it will
eventually need refreshing from a fresh capture.

**Recognizing an expired token, confirmed live:** every command starts failing with
`CROSS-SERVER LOGIN FAILED: ec=28 full={ep=[E011], ec=28}` — the connection succeeds, but login
itself is rejected, so nothing downstream even gets attempted. Don't confuse this with a single
flaky run: it was 100% reproducible across 16 consecutive scheduled runs (every 3 hours for ~42
hours) until the credentials were refreshed. Fixing it needs a fresh capture, same as initial setup
— and in the one real recurrence so far, `shumeiBoxId` had also changed, not just `accessToken`, so
re-extract both from the new capture rather than assuming only the token moved.

**This file contains live credentials for a real account — keep it out of version control** (it's
already outside the repo, in your home directory, and `chmod 600`'d above; don't move it into
this repo or commit it anywhere).

## Usage

```
# One-time setup: see "Session config" above, then everything below just works with no flags.

# Collect resources from every confirmed building type, plus the Armed Truck idle reward,
# greeting city visitors, helping alliance members, claiming all mail and alliance gifts,
# donating to the recommended alliance tech, and both once-a-day VIP claims:
./lastwar-client -collect

# Just list buildings without collecting:
./lastwar-client -list-buildings

# Stay connected and issue ad-hoc test commands without re-authenticating:
mkfifo /tmp/lw_cmd_pipe
./lastwar-client -interactive /tmp/lw_cmd_pipe &
echo 'building.production.collect {"uuid":123}' > /tmp/lw_cmd_pipe

# Override specific config fields for a one-off test (e.g. a different captured token):
./lastwar-client -collect -cs-at <a-different-access-token>

# Brand-new guest account instead (always works, no email or config needed):
./lastwar-client -list-buildings -no-config

# Bind a guest session to a real account via email verification (only needed once,
# to obtain a fresh config -- see docs/capturing-and-decoding-traffic.mdx for turning
# that into a session config):
mkfifo /tmp/lw_code_pipe
./lastwar-client -email you@example.com -code-pipe /tmp/lw_code_pipe &
echo 123456 > /tmp/lw_code_pipe
```

Device identity also persists across runs in `~/.lastwar_goclient_*` (deviceId, username, gameUid,
loginKey) independent of the session config, so repeated guest/email-flow runs present a consistent
device to the server. Delete those files to start fully fresh.

## Running unattended (cron)

Confirmed live: `-collect` on a schedule, on a separate machine from wherever the session config
was captured. The binary is a single static executable, so this is just cross-compiling and
copying two files:

```bash
# Build for the target machine (adjust GOOS/GOARCH -- this example is Linux x86_64):
GOOS=linux GOARCH=amd64 go build -o lastwar-client-linux-amd64 .

# Copy the binary and the session config (the binary is useless without it):
scp lastwar-client-linux-amd64 user@host:~/lastwar-client/lastwar-client
scp ~/.lastwar_goclient_session.json user@host:~/.lastwar_goclient_session.json
ssh user@host 'chmod +x ~/lastwar-client/lastwar-client; chmod 600 ~/.lastwar_goclient_session.json'

# Cron entry -- set HOME explicitly, cron's default environment can't be assumed to have it:
ssh user@host "cat <<'CRONEOF' | crontab -
HOME=/home/user
0 */3 * * * /home/user/lastwar-client/lastwar-client -collect >> /home/user/lastwar-client/logs/collect.log 2>&1
CRONEOF"
```

Two things worth checking after setup, not just once but as ongoing habits:

- **Actually confirm cron itself fires the job**, not just that the binary runs when you invoke it
  manually over SSH — those aren't the same test. `crontab -l` accepting the entry doesn't prove
  the daemon is running or the schedule is right. Add a one-off entry a couple of minutes out,
  wait for it, and check both the log file *and* cron's own record of running it
  (`grep CRON /var/log/syslog` on Debian/Ubuntu) before trusting the real schedule.
- **The log file has no rotation** in the example above — it just appends forever. At 8 runs/day
  it takes a long time to matter, but for a long-lived deployment either cap it with `logrotate`
  or watch its size periodically.
- **Check the log's error rate periodically, not just whether the process is still running.** A
  cron job can "work" (exit 0, get invoked on schedule) while every run inside it is failing at
  the login step — see the token-expiry note in "Session config" above. Grepping the log for
  `"level":"ERROR"` and checking *when* errors started (not just whether any exist) is what
  actually catches this — see `docs/live-validation.mdx`'s serverInfo-redirect section for a
  real example of a failure that looked identical on every single run once it started.
- **Exit code 2 means the session itself is stale, not a transient blip.** Login/auth failures
  (both the plain-login and cross-server-reconnect paths) exit `2` specifically, distinct from
  the generic exit `1` used for other failures -- a cron wrapper can check `$?` directly and
  know to recapture a fresh session (see "Session config" above) without needing to grep the log
  at all.
- **`-log-level` controls the JSON log verbosity** (`debug`, `warn`, or `error`; default `info`) --
  handy for trimming a noisy cron log down to warnings/errors only, or turning on `debug` output
  while chasing down a problem run.

## Files

| File | Purpose |
|---|---|
| `crypto.go` | GSL RSA-PKCS1v15 + AES-256-ECB-PKCS7 request/response envelope |
| `sfsobject.go` | SFSObject/SFSArray binary codec (encode + decode), recursive pretty-printing; `StringRedacted` is `String`'s safe-to-log twin, masking known-sensitive fields (`loginKey`/`at`/`rt`/`accessToken`/`airKey`/`shumeiBoxId`/`pw`/`password`/`verifyCode`/`deviceId`/`chatToken`/`tk`) -- including inside a sensitive key's own primitive-array value -- so a decoded response can be logged for debugging without risking a credential leak |
| `packet.go` | SFS2X packet framing: header flags, length-derived XOR, zlib compression, Zstandard decompression |
| `gsl.go` | HTTP bootstrap: check-version, GSL `getserverlist.php` (`opt=new\|login\|fix\|refresh`) |
| `conn.go` | TCP connection, envelope send/receive, heartbeat |
| `errors.go` | `ErrAuthRejected` sentinel distinguishing exit code 2 (confirmed auth rejection) from exit code 1 (generic failure) |
| `identity.go` | Persisted device identity + the ~50-field SFS `Login` params object (Android and iOS variants) |
| `config.go` | Session config file loading (`~/.lastwar_goclient_session.json` / `-config`) |
| `login.go` | Full login orchestration: guest login, email verification, account binding |
| `crossserver.go` | Direct role reconnect (`CrossServerLogin` reimplementation); `LWDEBUG_DUMP_LOGIN`/`LWDEBUG_DUMP_LOGIN_BODY` env vars dump the outgoing login request for debugging (the latter writes it, live access token included, to a caller-chosen file at 0600 -- same sensitivity as the session config) |
| `buildings.go` | Building list parsing, type-ID table, resource-collection commands |
| `visitors.go` | City-visitor parsing (from the `init` push) and greeting (`visitor.operate`) |
| `mail.go` | Mailbox listing/pagination and batch reward claiming, scoped per category |
| `alliance.go` | Alliance automation: help-all, gift claiming, recommended-tech donation |
| `vip.go` | Once-a-day VIP claims: login-streak score and the VIP-level daily freebie chest |
| `interactive.go` | Control-FIFO REPL for testing commands against a live session without re-login |
| `decode.go` | `-decode-stream`: decode a reassembled capture file with the live codec, no network |
| `main.go` | CLI entrypoint |
| `selftest_test.go` | Unit tests for the crypto/codec/framing layers (no network required) |
| `crypto_gsl_test.go` | GSL crypto envelope round trip: `NewGSLCrypto` + `EncryptRequest` + `DecryptResponse` composed together, not just the AES-ECB/PKCS7 primitives `selftest_test.go` covers |
| `gsl_http_test.go` | `CheckVersion` and `GetServerList` exercised against a fake HTTP server; `GetServerList`'s four response-error branches (HTTP status, top-level JSON, decrypted plaintext, plaintext fallback) proven to never leak the raw body/decrypted plaintext -- which can carry a live `at`/`rt` session token -- into the returned error |
| `gsl_form_sync_test.go` | Regression test proving `encodeFormSorted`'s `order` whitelist stays in sync with `GetServerList`'s actual `form.Set(...)` calls, by reading gsl.go's own source for both (the same source-scanning technique `main_flags_test.go`'s `TestCrossServerFlagNamesMatchesDeclarations` uses against main.go) -- catches the exact silently-dropped-field drift `encodeFormSorted`'s doc comment records a prior production bug from |
| `packet_bigsized_test.go` | Packet framing round trip for payloads over 65535 bytes (4-byte length prefix, `hdrBigSized`) |
| `packet_zstd_test.go` | `ReadPacket`'s Zstandard-decompression branch (`hdrCompressed\|hdrUseLZ4`) round trip |
| `packet_oom_test.go` | `ReadPacket`'s declared-length size guards reject an oversized/hostile frame using only the header fields, before ever reading (let alone allocating) the body |
| `sfsobject_array_test.go` | Array-tag decode round trips (including `ByteArray`'s 4-byte element count vs. every other array tag's 2-byte count) and encode round trips; a hostile-input battery against the decoder -- negative array/text counts, the `maxNestDepth` recursion bomb, the `maxDecodedNodes` wide-fan-out amplification bomb, and `DecodeObject` rejecting trailing bytes left over after a well-formed top-level object |
| `sfsobject_encode_error_test.go` | `EncodeObject` returns an error instead of panicking when a string value exceeds the wire format's 65535-byte length-prefix limit, including through nested-`SFSObject` recursion |
| `sfsobject_redact_test.go` | `StringRedacted` masks every known-sensitive key (`loginKey`/`at`/`rt`/`accessToken`/`airKey`/`shumeiBoxId`/`pw`/`password`/`verifyCode`/`deviceId`/`chatToken`/`tk`, including inside nested `SFSObject`/`SFSArray` values and inside a sensitive key's own primitive-array value) while leaving ordinary gameplay fields untouched, and matches `String`'s output exactly when no sensitive keys are present |
| `conn_test.go` | `Envelope.AsExtension`, `classifyResponse`'s success/benign/failure outcome classification, and a `GameConn` send/receive round trip |
| `conn_wait_test.go` | `sendAndWait`/`waitFor`/`waitForCmd`/`waitForInitPush`: outcome classification, deadline timeouts, unmatched-push skipping, and the init-push halfway active-pull fallback; `waitFor`'s "skipped push while waiting" debug logger proven to never leak a raw `loginKey` when `push.account.login.new` is the message being skipped |
| `conn_handshake_test.go` | `DoHandshake`'s success path and its `ec`-bearing failure path wrapping `ErrAuthRejected`; `StartHeartbeat`'s periodic pings, stop-on-`Close`, and close-on-send-failure behavior; the "skipped envelope while waiting for handshake" fallback log proven to never leak a raw credential |
| `identity_test.go` | `BuildLoginParams`' Android/iOS and empty-vs-set-`GameUid` conditional field logic; `SaveLoginKey`/loose-permission warning and load/save round trip for the persisted device identity; `stateFilePath`'s `$HOME`-unavailable fallback proven to log a warning, not fail silently |
| `main_test.go` | `parseLogLevel`'s recognized values and its unrecognized-value fallback-to-info behavior |
| `main_flags_test.go` | `decodeModeIgnoredFlags` and `ignoredCrossServerFlags`'s exempt-flag filtering (`decode-stream`/`decode-label`/`log-level`, and the redirect-gating `cs-ip`/`cs-rt`, are never reported as ignored); `refreshHasUsableData`'s At-token-or-nonempty-ServerList check gating whether a GSL `opt=refresh` response is usable; a regression test scanning main.go's own `fs.String/Bool/Int("cs-...", ...)` declarations to keep `crossServerFlagNames` in sync with the FlagSet as flags are added, renamed, or removed |
| `main_crossserver_test.go` | `crossServerSaveBackNeeded` (the pure comparison extracted from `runCrossServerTest`'s session-config save-back check), including the round-12 regression case where only the access token changed (a `-cs-rt` refresh with no `serverInfo` redirect) -- previously silently dropped since the save-back condition never compared `AccessTok` at all; an end-to-end `runCrossServerTest` test proving a `-cs-rt` refresh's fresh access token is actually persisted to the session config file even when no redirect occurs |
| `config_test.go` | Session config load/save: explicit-path loading, loose-file-permission warnings, permission tightening on save |
| `login_test.go` | `gslOptFor`'s 3-way opt-priority order (`login` > `fix` > `new`, including the case where `LoginKey` and `GameUid` are both set); `redact`'s secret-masking for log output |
| `login_integration_test.go` | `Login()` exercised end to end against a fake CheckVersion/GetServerList HTTP endpoint and `net.Listen`-based fake SFS2X game servers (reusing crossserver_test.go's fake-server helpers): the guest happy-path dial+login+init-push flow with no redirect; serverInfo-redirect handling refreshing `gameUid` mid-redirect onto both the returned `Ident` and the persisted identity file (the `Login()` counterpart to crossserver_test.go's `DoCrossServerLogin` redirect test); the `maxRedirectHops`-exceeded bound (also mirroring its `DoCrossServerLogin` counterpart); the email-verification path (send-code/wait-for-code/account.login.new/push.account.login.new) persisting a real, non-guest loginKey/gameUid/username; and `Login()`'s captured log output proven to never contain the raw verification code, deviceId, or airKey (only their lengths) |
| `buildings_visitors_test.go` | `BuildingNameOf`, `collectCmdFor`, and init-push building/visitor parsing (`ParseInitBuildings`/`ParseInitVisitors`, including malformed-entry skipping) |
| `pure_helpers_test.go` | Pure helpers spanning buildings/mail/alliance: `collectibleBuildings`, `groupUnclaimedByType`, `findRecommendedTech`; `Mail.HasUnclaimedReward`'s explicit-null-vs-missing `rewardStatus` guard, proving a genuinely-absent field (notification-only mail) is excluded rather than misclassified as unclaimed |
| `redirect_helpers_test.go` | GSL/cross-server helpers: `findServerInfo`, `getIntFlexible`, `serverIDFromZone` |
| `interactive_test.go` | `putJSONValue`'s JSON-to-`SFSObject` type mapping, including `json.Number`'s int64-vs-float64 fallback for uuid-sized values |
| `alliance_test.go` | `HelpAllianceMembers` and `ClaimAllianceGifts` (both gift types, Premium then Regular, in order); `findRecommendedTech`'s state==1/scienceId selection, including the regression where an explicit-null `scienceId` must be skipped rather than falling through to `scienceId=0`; and `DonateRecommendedAllianceTech`'s three no-op branches plus its real donate call |
| `crossserver_test.go` | `DoCrossServerLogin` against real `net.Listen`-based fake servers (it dials its own connection, so it can't use `net.Pipe` like the other orchestration tests): the no-redirect success path, following a single `serverInfo` redirect, the `maxRedirects` bounded-loop guard erroring out instead of looping forever, the mid-redirect GSL refresh propagating a fresh `gameUid` into both the top-level `un` field and the nested login-params `gameUid` field, the `LWDEBUG_DUMP_LOGIN` debug dump proven to never leak the raw access token/`shumeiBoxId`, and the experimental `-handshake` path's "handshake OK" log proven to never leak the Handshake response's `tk` session token |
| `mail_orchestration_test.go` | `ListMail`'s pagination loop (`lastUid`/`lastMailTime` carried into the next request, `firstCmd` sent only on the cold-start page), a `more=true` response with a missing `lastUid` correctly stopping pagination instead of looping on a stale cursor, and `ClaimAllMail`'s batching under both the 100-item count cap and the 60000-byte length cap, proving batches split at the right boundary with no uid dropped, duplicated, or reordered |
| `buildings_orchestration_test.go` | `FetchBuildings` parsing an `init` push's `building_new`/`visitor` fields and its timeout behavior with no data and with partial data; building uuids deduped across the `init`/`push.init.build`/`push.add.building` sources so a redundant collect call is never issued twice for the same building; its unrecognized-push log lines proven to never leak a raw credential; `CollectIdleReward`'s peek-then-claim two-call sequence; `CollectAll` aggregating a genuine mid-sequence failure (`al.help.all`) without short-circuiting the remaining calls |
| `visitors_orchestration_test.go` | `GreetVisitors`: the empty-visitor-list short-circuit (nothing sent), one `visitor.operate` request per visitor in order, and error aggregation that folds away the benign `visitor_err_coming` errorCode while still surfacing a genuine failure, without stopping at the first error |
| `interactive_orchestration_test.go` | `handleInteractiveLine` over a `net.Pipe`-backed connection: aborts without sending anything when a parsed JSON value has no `putJSONValue` case, correctly parses/sends a well-formed `cmd.name {json}` line as an Extension call, and its "sending command"/"received response" log lines are proven to never leak a raw `loginKey`/`pw` from either the outgoing params or the incoming response |
| `decode_test.go` | `DecodeStreamFile`'s three branches against real `EncodePacket(EncodeObject(...))` output written to a temp file: clean end-of-stream after well-formed packets, a truncated-frame error naming the byte offset, and a corrupt-`SFSObject`-body packet whose `DecodeObject` error is logged inline while the stream continues decoding the packets that follow rather than aborting; a `push.account.login.new`-shaped packet proven to never print its raw `loginKey` to the decoded-stream output |
| `credential_leak_lint_test.go` | A repo-wide, recursive-directory regex scan of every non-test `.go` file for a `slog.*`/`fmt.Errorf/Printf/Fprintf/Sprintf/Println`/`log.Print*` call that embeds a raw `.String()` call (joining multi-line calls first) -- the credential-leak pattern this repo has hit repeatedly -- failing the build on any new, un-allowlisted instance instead of waiting for the next audit round to find it |
| `tools/reassemble_stream.py` | Reassembles one TCP stream from a pcap into `-decode-stream`-ready files — see [Capturing and decoding traffic](docs/capturing-and-decoding-traffic.mdx) |

## License

Apache License 2.0 -- see [LICENSE](LICENSE).
