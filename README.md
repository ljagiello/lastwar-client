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

# Stay connected and issue ad-hoc test commands without re-authenticating. NOTE: since
# -collect isn't passed here, the full building list still prints to stdout once at
# startup by default (this is true of every run that omits -collect, not just -interactive
# ones -- pass -collect to suppress it, or -list-buildings to make the print explicit).
# Only flat scalar params (strings/bools/numbers) are supported over this control FIFO --
# nested/array-shaped params (e.g. the "heroes" array docs/military-battle.mdx documents for
# StartArenaBattleMessage) can't be exercised this way today:
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
| `crypto.go` | GSL RSA-PKCS1v15 + AES-256-ECB-PKCS7 request/response envelope; `pkcs7Unpad` rejects a claimed pad length larger than the block size instead of only bounding it by the ciphertext length |
| `sfsobject.go` | SFSObject/SFSArray binary codec (encode + decode), recursive pretty-printing. Both `SFSObject` and `SFSArray` are safe by default: `String`/`GoString` delegate to `StringRedacted`, so Go's automatic `fmt.Stringer`/`fmt.GoStringer` invocation (`%v`/`%s`/`%#v`, `Println`, slog's Any-kind formatting) can never leak a raw dump; a bare `*SFSArray` reached with no enclosing key to check (no context to lean on) blanket-masks every item, while an array reached as a value already sitting under a known-non-sensitive key stays fully readable for debugging. `StringRedacted` is fail-closed: it explicitly masks known-sensitive fields (`loginKey`/`at`/`rt`/`accessToken`/`airKey`/`shumeiBoxId`/`pw`/`password`/`verifyCode`/`deviceId`/`chatToken`/`tk`/`ta`/`mail`/`gcmRegisterId`/`parseRegisterId`/`googleName`/`mt`/`simOp`/`simOpName`/`phone_model`/`osVersion`/`phone_screen`/`phone_native_screen`, plus the device/ad-identifier PII cluster `IMEI`/`AndroidID`/`androidDid`/`idfa`/`idfv`/`gaid`/`afuid`/`firebaseId`/`distinct_id`), matched case-insensitively via `isSensitiveSFSKey` so a mis-cased operator-typed key (e.g. `-interactive`'s `LoginKey` vs. the registered `loginKey`) still gets redacted, including inside a sensitive key's own primitive-array, `SFSArray`-wrapped, or nested-`SFSObject` value, gracefully on a nil nested value instead of panicking -- and any value shape under a sensitive key it doesn't explicitly recognize as safe falls through to a fixed `[REDACTED]` placeholder rather than to a formatter that might print real content. `EncodeObject`/`writeValuePayload` return a clean error instead of panicking on a nil nested `*SFSObject`/`*SFSArray`. `maxDecodedNodes` charges each primitive array's actual element count (not a flat 1 per field) against the decode-size budget, except `sfsByteArray`, whose 1:1 wire-to-memory ratio needs no separate charge |
| `packet.go` | SFS2X packet framing: header flags, length-derived XOR, zlib compression, Zstandard decompression; every field read after the leading header byte converts a bare `io.EOF` into `io.ErrUnexpectedEOF`, so a capture truncated mid-frame is never misreported as a clean end-of-stream |
| `gsl.go` | HTTP bootstrap: check-version, GSL `getserverlist.php` (`opt=new\|login\|fix\|refresh`); `LoginServerListRespon.Code` is `flexString` (tolerates both JSON-string and bare-number shapes, matching its sibling `CheckVersionResponse.Code`); `encodeFormSorted` errors instead of silently dropping a form field absent from its `order` whitelist; `applyLoginServerFallback` synthesizes a `ServerList` entry from `LoginServer` when `opt=new` returns an empty `ServerList`, a conservative fallback for a scenario never yet observed live |
| `conn.go` | TCP connection, envelope send/receive, heartbeat |
| `errors.go` | `ErrAuthRejected` sentinel distinguishing exit code 2 (confirmed auth rejection) from exit code 1 (generic failure) |
| `identity.go` | Persisted device identity + the ~50-field SFS `Login` params object (Android and iOS variants); the iOS-only `ta` analytics blob is diagnostic-only placeholders, not the real deviceId/airKey/shumeiBoxId values, since that blob is a single opaque JSON string `StringRedacted` can only mask wholesale; `loadOrCreateDeviceIdentity` no longer treats a non-ENOENT read failure on an existing state file as "no identity yet" (which used to silently fabricate and persist a replacement device ID), and creates a fresh device-ID file with `O_EXCL` instead of an unconditional overwrite; a pre-existing-but-empty state file (e.g. left behind by a process that crashed between `O_CREATE\|O_EXCL` and writing its content) self-heals with a fresh identity via an atomic write-temp-then-rename after a short bounded retry window gives any genuine concurrent writer a chance to finish first, instead of permanently failing every subsequent run |
| `config.go` | Session config file loading (`~/.lastwar_goclient_session.json` / `-config`); `loadEffectiveConfig` fails loudly (`os.Exit(1)`) on any non-`ENOENT` read error on either an explicit `-config` path or the default path, instead of silently treating a permission/I-O glitch on an existing file the same as "no config yet" and diverting into an unrelated guest/email login flow |
| `login.go` | Full login orchestration: guest login, email verification, account binding; the operator's email address is logged only as a length (`emailLen`), never in cleartext; `waitForInitPush` returns its terminal read error so a real connection failure while waiting for `init` is logged distinctly from an ordinary timeout, and `Login` now closes the connection and returns an error immediately on that case instead of falling through to a nominally-successful result wrapping a dead connection; `buildBaseZoneLoginAddr` fails clearly instead of silently dialing the loopback interface when the resolved ip is empty, mirroring `main.go`'s cross-server guard, and is now also used by `Login`'s own mid-login `serverInfo` redirect branch (previously unguarded there) as well as the initial dial |
| `crossserver.go` | Direct role reconnect (`CrossServerLogin` reimplementation); `LWDEBUG_DUMP_LOGIN`/`LWDEBUG_DUMP_LOGIN_BODY` env vars dump the outgoing login request for debugging (the latter writes it, live access token included, to a caller-chosen file explicitly `chmod`'d to 0600 after every write, not just on creation -- same sensitivity as the session config); `DoCrossServerLogin`'s own mid-login `serverInfo` redirect branch now routes through `login.go`'s `buildBaseZoneLoginAddr` (instead of an unguarded `firstHost(...)`-based address), failing clearly instead of silently dialing loopback on a malformed redirect ip |
| `buildings.go` | Building list parsing, type-ID table, resource-collection commands; `FetchBuildings` dedupes visitors across repeated `init` pushes the same way it already dedupes buildings; `CollectAll` aborts its remaining sub-actions early on a genuine connection-level (`net.Error`) failure instead of still waiting out every remaining action's timeout in turn |
| `visitors.go` | City-visitor parsing (from the `init` push) and greeting (`visitor.operate`); `GreetVisitors` aborts its remaining (`maxNum`-bounded) visitors immediately on a `net.Error`, instead of burning a full command timeout per remaining visitor against an already-dead connection |
| `mail.go` | Mailbox listing/pagination and batch reward claiming, scoped per category; `groupUnclaimedByType` skips (with a warning) any reward-bearing mail whose `type` field is missing/explicit-null instead of defaulting it into a `type=0` batch; `ListMail` warns when it exhausts its 20-page pagination cap while the server still reports more mail available, instead of silently truncating; `ClaimAllMail` now processes whatever partial mail `ListMail` already collected before a mid-pagination failure, instead of discarding it; both `ClaimAllMail` batch loops (read-status and per-type reward-claim) now abort their remaining batches immediately on a `net.Error` instead of burning a full command timeout per remaining batch against an already-dead connection; `ClaimAllMail` also checks `ListMail`'s own returned error for a `net.Error` before entering the read-status loop at all, skipping straight to returning instead of issuing one more batch against an already-known-dead connection |
| `alliance.go` | Alliance automation: help-all, gift claiming, recommended-tech donation; `ClaimAllianceGifts` aborts its remaining gift type immediately on a `net.Error`, instead of also attempting it against an already-dead connection |
| `vip.go` | Once-a-day VIP claims: login-streak score and the VIP-level daily freebie chest -- see `vip_test.go` |
| `interactive.go` | Control-FIFO REPL for testing commands against a live session without re-login; a malformed JSON command logs only `cmd` and the parse error, never the operator's raw unparsed params text; trailing data after a well-formed JSON value now aborts the send with an error instead of being silently discarded |
| `decode.go` | `-decode-stream`: decode a reassembled capture file with the live codec, no network; a `DecodeObject` failure now prints only the error and the raw body's byte length, never a hex dump of the (pre-decode, unredacted) body content -- a truncated-but-otherwise-well-formed frame could have an intact sensitive field sorted near the front, which the old hex dump bypassed `sensitiveSFSKeys` entirely to expose |
| `main.go` | CLI entrypoint; warns when `-decode-label` is set without `-decode-stream` (a no-op combination, mirroring this file's other ignored-flag warnings); `runCrossServerTest` fails clearly (rather than silently dialing the loopback interface) when the resolved ip is empty, mirroring its existing port validation; `-interactive`'s help text documents its flat-scalar-only param limitation; a GSL `opt=refresh` response with no usable data now exits 2 (matching the README's own documented stale-session cron-exit-code contract), not the generic 1; `runCrossServerTest` warns at WARN (not silently at INFO) when a GSL refresh response's server list overrides an explicitly-passed `-cs-ip`/`-cs-port`/`-cs-zone`/`-cs-gameuid` flag, naming which flag(s) were overridden; the "ignoring -cs-at"/"continuing ... unrefreshed" warnings now only attribute the value to the `-cs-at` flag when it was actually typed on the command line, not when it came from a loaded session config (reusing the `-cs-ios`-style `fs.Visit` explicit-flag tracking) |
| `selftest_test.go` | Unit tests for the crypto/codec/framing layers (no network required); `pkcs7Unpad` rejecting a claimed pad length above the AES block size on a corrupted multi-block plaintext |
| `crypto_gsl_test.go` | GSL crypto envelope round trip: `NewGSLCrypto` + `EncryptRequest` + `DecryptResponse` composed together, not just the AES-ECB/PKCS7 primitives `selftest_test.go` covers |
| `gsl_http_test.go` | `CheckVersion` and `GetServerList` exercised against a fake HTTP server; `GetServerList`'s four response-error branches (HTTP status, top-level JSON, decrypted plaintext, plaintext fallback) proven to never leak the raw body/decrypted plaintext -- which can carry a live `at`/`rt` session token -- into the returned error; a string-typed `code` field decodes successfully via `LoginServerListRespon.Code`'s `flexString` type; `applyLoginServerFallback` synthesizing a `ServerList` entry from `LoginServer` for `opt=new` with an empty `ServerList`, and proven scoped to `opt=new` only (an `opt=fix` response with the same shape stays empty) |
| `gsl_form_sync_test.go` | Regression test proving `encodeFormSorted`'s `order` whitelist stays in sync with `GetServerList`'s actual `form.Set(...)` calls, by reading gsl.go's own source for both (the same source-scanning technique `main_flags_test.go`'s `TestCrossServerFlagNamesMatchesDeclarations` uses against main.go) -- catches the exact silently-dropped-field drift `encodeFormSorted`'s doc comment records a prior production bug from; `encodeFormSorted`'s own runtime backstop (erroring when a `form` field is absent from `order`) is exercised directly too |
| `packet_bigsized_test.go` | Packet framing round trip for payloads over 65535 bytes (4-byte length prefix, `hdrBigSized`) |
| `packet_zstd_test.go` | `ReadPacket`'s Zstandard-decompression branch (`hdrCompressed\|hdrUseLZ4`) round trip |
| `packet_oom_test.go` | `ReadPacket`'s declared-length size guards reject an oversized/hostile frame using only the header fields, before ever reading (let alone allocating) the body; a stream truncated exactly on a field-read boundary mid-frame (the shape a real cut-off capture produces) is proven to surface as `io.ErrUnexpectedEOF`, not be misclassified as a clean end-of-stream |
| `sfsobject_array_test.go` | Array-tag decode round trips (including `ByteArray`'s 4-byte element count vs. every other array tag's 2-byte count) and encode round trips; a hostile-input battery against the decoder -- negative array/text counts, the `maxNestDepth` recursion bomb, the `maxDecodedNodes` wide-fan-out amplification bomb (including the per-element-charged primitive-array variant), and `DecodeObject` rejecting trailing bytes left over after a well-formed top-level object |
| `sfsobject_encode_error_test.go` | `EncodeObject` returns an error instead of panicking when a string value exceeds the wire format's 65535-byte length-prefix limit, including through nested-`SFSObject` recursion |
| `sfsobject_redact_test.go` | `StringRedacted` masks every known-sensitive key (`loginKey`/`at`/`rt`/`accessToken`/`airKey`/`shumeiBoxId`/`pw`/`password`/`verifyCode`/`deviceId`/`chatToken`/`tk`/`ta`/`mail`/`gcmRegisterId`/`parseRegisterId`/`googleName`/`mt`/`simOp`/`simOpName`/`phone_model`/`osVersion`/`phone_screen`/`phone_native_screen`, plus the device/ad-identifier PII cluster, including inside nested `SFSObject`/`SFSArray` values, a sensitive key's own raw `SFSArray`-wrapped value, and all 8 primitive-array wire types) while leaving ordinary gameplay fields untouched, matches `String`'s output exactly when no sensitive keys are present, `BuildLoginParams(IOSMode: true)` proven to never embed live DeviceID/AirKey/ShumeiBoxId secrets in its `ta` analytics blob, `fmt.Sprintf("%v"/"%#v", ...)` on a raw `*SFSObject` proven to never leak via Go's automatic `fmt.Stringer`/`fmt.GoStringer` invocation, `%v`/`%s`/`%#v`/`Fprintln` on a bare `*SFSArray` proven to never leak a raw scalar item, a nil nested value proven to never panic, a legitimate large single `sfsByteArray` field proven to no longer spuriously fail against `maxDecodedNodes`, `redactSFSValue`'s fail-closed fallback proven to mask a scalar (`PutInt`/`PutLong`/`PutBool`/`PutDouble`), a decode-only `sfsFloat`/`sfsNull` value, or a nested `*SFSObject`'s own sub-fields under a sensitive key, `EncodeObject` returning a clean error (not panicking) on a nil nested `*SFSObject`/`*SFSArray` value, and a case-variant sensitive key (`LoginKey`/`LOGINKEY`/`lOgInKeY`, ...) still getting redacted via `isSensitiveSFSKey`'s case-insensitive lookup |
| `sfsobject_sensitive_keys_sync_test.go` | Self-enforcing completeness check tying `sensitiveSFSKeys` (sfsobject.go) to reality: statically scans every non-test `.go` file for a literal `Put*("key", ...)` call site (mirroring `gsl_form_sync_test.go`'s source-scanning technique) and fails loudly, naming the key and call site, if it lands in neither `sensitiveSFSKeys` nor this file's own reviewed `knownNonSensitiveSFSKeys` allowlist -- turning the "does sensitiveSFSKeys cover every field this repo actually sends?" question five straight audit rounds answered by manual re-audit into a CI check; also guards `knownNonSensitiveSFSKeys` against drift if a listed key is later promoted into `sensitiveSFSKeys` |
| `conn_test.go` | `Envelope.AsExtension`, `classifyResponse`'s success/benign/failure outcome classification, and a `GameConn` send/receive round trip |
| `conn_wait_test.go` | `sendAndWait`/`waitFor`/`waitForCmd`/`waitForInitPush`: outcome classification, deadline timeouts, unmatched-push skipping, and the init-push halfway active-pull fallback; `waitFor`'s "skipped push while waiting" debug logger proven to never leak a raw `loginKey` when `push.account.login.new` is the message being skipped; `waitForInitPush` returning its terminal error distinctly from a genuine timeout; `buildBaseZoneLoginAddr`'s empty-ip guard, including against a `|`-delimited fallback list whose first entry is empty; the init-push halfway active-pull timing assertion widened to an eighth/seven-eighths band to absorb CI scheduler jitter; and `Login()`'s own mid-login `serverInfo` redirect branch rejecting a malformed/empty redirect ip clearly instead of silently dialing loopback |
| `conn_handshake_test.go` | `DoHandshake`'s success path and its `ec`-bearing failure path wrapping `ErrAuthRejected`; `StartHeartbeat`'s periodic pings, stop-on-`Close`, and close-on-send-failure behavior; the "skipped envelope while waiting for handshake" fallback log proven to never leak a raw credential |
| `identity_test.go` | `BuildLoginParams`' Android/iOS and empty-vs-set-`GameUid` conditional field logic; `SaveLoginKey`/loose-permission warning and load/save round trip for the persisted device identity; `stateFilePath`'s `$HOME`-unavailable fallback proven to log a warning, not fail silently; `loadOrCreateDeviceIdentity` proven to never silently fabricate a replacement device ID on a non-ENOENT state-file read failure; a pre-existing-but-empty (no concurrency involved) device-id file proven to self-heal with a fresh identity instead of permanently failing every subsequent run; a genuine concurrent writer's content proven to still be adopted, not overwritten by the self-heal path |
| `main_test.go` | `parseLogLevel`'s recognized values and its unrecognized-value fallback-to-info behavior; the flag-parsing exit-code contract (`-h`/`-help` exits 0, an unrecognized/malformed flag exits 1) via a re-exec'd subprocess |
| `main_flags_test.go` | `decodeModeIgnoredFlags` and `ignoredCrossServerFlags`'s exempt-flag filtering (`decode-stream`/`decode-label`/`log-level`, and the redirect-gating `cs-ip`/`cs-rt`, are never reported as ignored); `refreshHasUsableData`'s At-token-or-nonempty-ServerList check gating whether a GSL `opt=refresh` response is usable; a regression test scanning main.go's own `fs.String/Bool/Int("cs-...", ...)` declarations to keep `crossServerFlagNames` in sync with the FlagSet as flags are added, renamed, or removed; `warnIfDecodeLabelIgnored`'s warning firing only when `-decode-label` is set without `-decode-stream` |
| `main_crossserver_test.go` | `crossServerSaveBackNeeded` (the pure comparison extracted from `runCrossServerTest`'s session-config save-back check), including the round-12 regression case where only the access token changed and the round-13 case where only `GameUid` changed (a `-cs-rt` refresh with no `serverInfo` redirect) -- both previously silently dropped since the save-back condition didn't compare them; an end-to-end `runCrossServerTest` test proving a `-cs-rt` refresh's fresh access token is actually persisted to the session config file even when no redirect occurs; `runCrossServerTest` exiting clearly (not silently dialing loopback) when the resolved ip is empty; a GSL `opt=refresh` response with no usable data exiting 2 (not 1); a `-cs-rt` refresh's server list overriding an explicitly-passed `-cs-ip`/etc. flag logging at WARN naming the overridden flag(s), versus a plain INFO when nothing explicit was overridden |
| `config_test.go` | Session config load/save: explicit-path loading, loose-file-permission warnings, permission tightening on save; `loadEffectiveConfig` returning `(nil, "")` silently on a genuinely absent default path, and exiting with a clear error (via a re-exec'd subprocess, asserting exit code and stderr) on a non-`ENOENT` read failure (a directory sitting where the file is expected) instead of silently treating it the same as absent |
| `login_test.go` | `gslOptFor`'s 3-way opt-priority order (`login` > `fix` > `new`, including the case where `LoginKey` and `GameUid` are both set); `redact`'s secret-masking for log output |
| `login_integration_test.go` | `Login()` exercised end to end against a fake CheckVersion/GetServerList HTTP endpoint and `net.Listen`-based fake SFS2X game servers (reusing crossserver_test.go's fake-server helpers): the guest happy-path dial+login+init-push flow with no redirect; serverInfo-redirect handling refreshing `gameUid` mid-redirect onto both the returned `Ident` and the persisted identity file (the `Login()` counterpart to crossserver_test.go's `DoCrossServerLogin` redirect test); the `maxRedirectHops`-exceeded bound (also mirroring its `DoCrossServerLogin` counterpart); the email-verification path (send-code/wait-for-code/account.login.new/push.account.login.new) persisting a real, non-guest loginKey/gameUid/username; and `Login()`'s captured log output proven to never contain the raw verification code, deviceId, airKey, or operator email address (only their lengths); a fake server that closes the connection instead of ever sending `init` proven to make `Login()` return a clear error immediately (the round-17 `initErr != nil` fix), not a nominally-successful result wrapping a dead connection |
| `buildings_visitors_test.go` | `BuildingNameOf`, `collectCmdFor`, and init-push building/visitor parsing (`ParseInitBuildings`/`ParseInitVisitors`, including malformed-entry skipping) |
| `pure_helpers_test.go` | Pure helpers spanning buildings/mail/alliance: `collectibleBuildings`, `groupUnclaimedByType`, `findRecommendedTech`; `Mail.HasUnclaimedReward`'s explicit-null-vs-missing `rewardStatus` guard and `groupUnclaimedByType`'s matching guard for a missing/null `type` field, both proving a genuinely-absent field (notification-only mail) is excluded rather than misclassified as a real zero value |
| `redirect_helpers_test.go` | GSL/cross-server helpers: `findServerInfo`, `getIntFlexible`, `serverIDFromZone` |
| `interactive_test.go` | `putJSONValue`'s JSON-to-`SFSObject` type mapping, including `json.Number`'s int64-vs-float64 fallback for uuid-sized values |
| `alliance_test.go` | `HelpAllianceMembers` and `ClaimAllianceGifts` (both gift types, Premium then Regular, in order, and aborting the 2nd type immediately on a `net.Error` while still attempting it on an ordinary decoded errorCode failure); `findRecommendedTech`'s state==1/scienceId selection, including the regression where an explicit-null `scienceId` must be skipped rather than falling through to `scienceId=0`; `DonateRecommendedAllianceTech`'s three no-op branches plus its real donate call; and the documented `al.science.donate` cooldown (`errorCode=120471`) benign-no-op classification |
| `crossserver_test.go` | `DoCrossServerLogin` against real `net.Listen`-based fake servers (it dials its own connection, so it can't use `net.Pipe` like the other orchestration tests): the no-redirect success path, following a single `serverInfo` redirect, the `maxRedirects` bounded-loop guard erroring out instead of looping forever, the mid-redirect GSL refresh propagating a fresh `gameUid` into both the top-level `un` field and the nested login-params `gameUid` field, the `LWDEBUG_DUMP_LOGIN` debug dump proven to never leak the raw access token/`shumeiBoxId` (including with `IOSMode: true`) or `LWDEBUG_DUMP_LOGIN_BODY`'s dump file always ending up `chmod`'d 0600 even when it pre-existed with looser permissions, the experimental `-handshake` path's "handshake OK" log proven to never leak the Handshake response's `tk` session token; and `DoCrossServerLogin`'s own mid-login `serverInfo` redirect rejecting a malformed/empty redirect ip clearly instead of silently dialing loopback |
| `mail_orchestration_test.go` | `ListMail`'s pagination loop (`lastUid`/`lastMailTime` carried into the next request, `firstCmd` sent only on the cold-start page), a `more=true` response with a missing `lastUid` correctly stopping pagination instead of looping on a stale cursor, a server that never stops reporting `more=true` correctly warning once the 20-page cap is reached instead of truncating silently, `ClaimAllMail` still processing already-collected mail after a mid-pagination `ListMail` failure instead of discarding it, `ClaimAllMail`'s batching under both the 100-item count cap and the 60000-byte length cap, proving batches split at the right boundary with no uid dropped, duplicated, or reordered, `ClaimAllMail` aborting all remaining read-status and reward-claim batches immediately on a `net.Error` (and skipping the reward-claim loop entirely when the read-status loop itself aborted that way), reward-claim batches for multiple distinct mail types still all running when only an ordinary decoded errorCode failure (not a `net.Error`) occurs, and `ListMail` itself failing with a `net.Error` after already collecting partial mail causing `ClaimAllMail` to return promptly without issuing any read-status batch call at all |
| `buildings_orchestration_test.go` | `FetchBuildings` parsing an `init` push's `building_new`/`visitor` fields and its timeout behavior with no data and with partial data; building uuids deduped across the `init`/`push.init.build`/`push.add.building` sources so a redundant collect call is never issued twice for the same building; visitor uids likewise deduped across repeated `init` pushes; its unrecognized-push log lines proven to never leak a raw credential; `CollectIdleReward`'s peek-then-claim two-call sequence; `CollectAll` aggregating a genuine mid-sequence failure (`al.help.all`) without short-circuiting the remaining calls, and aborting its remaining sub-actions early (proven via a fake connection whose every `Read` fails with a `net.Error`) instead of still waiting out every remaining timeout |
| `visitors_orchestration_test.go` | `GreetVisitors`: the empty-visitor-list short-circuit (nothing sent), one `visitor.operate` request per visitor in order, error aggregation that folds away the benign `visitor_err_coming` errorCode while still surfacing a genuine failure, without stopping at the first error, and aborting all remaining visitors immediately on a `net.Error` |
| `interactive_orchestration_test.go` | `handleInteractiveLine` over a `net.Pipe`-backed connection: aborts without sending anything when a parsed JSON value has no `putJSONValue` case, correctly parses/sends a well-formed `cmd.name {json}` line as an Extension call, its "sending command"/"received response" log lines are proven to never leak a raw `loginKey`/`pw` from either the outgoing params or the incoming response, a malformed-JSON command line is proven to never leak its raw unparsed params text (which could itself contain a credential the operator was about to send) into the "bad JSON params" error log, and trailing garbage after a well-formed JSON value aborts the send instead of being silently discarded |
| `decode_test.go` | `DecodeStreamFile`'s three branches against real `EncodePacket(EncodeObject(...))` output written to a temp file: clean end-of-stream after well-formed packets, a truncated-frame error naming the byte offset, and a corrupt-`SFSObject`-body packet whose `DecodeObject` error is logged inline while the stream continues decoding the packets that follow rather than aborting; a `push.account.login.new`-shaped packet proven to never print its raw `loginKey` to the decoded-stream output; and a truncated packet with an intact sensitive field (e.g. `tk`) ahead of the truncation point proven to never leak that field's value (raw or hex-encoded) via the `DecodeObject`-failure diagnostic line |
| `credential_leak_lint_test.go` | A repo-wide, recursive-directory regex scan of every non-test `.go` file for a `slog.*`/`fmt.*`/`log.*` sink call that embeds a raw `.String()`/`.unsafeRawString()` call (joining multi-line calls first, threading raw-string state across lines and stripping string/rune-literal contents and `//` comments before counting parens or matching so neither a stray `)` nor a multi-line backtick string can defeat the join) -- the credential-leak pattern this repo has hit repeatedly -- failing the build on any new, un-allowlisted instance instead of waiting for the next audit round to find it. Since round 14, `SFSObject.String()` itself is safe by default (see sfsobject.go), so this guard's live purpose is now catching a call to the `unsafeRawString()` escape hatch from outside sfsobject.go, with plain `.String()` still flagged for defense-in-depth |
| `vip_test.go` | `ClaimVIPDailyLoginScore` and `ClaimVIPDailyFreebie`: exact cmd string sent with empty params on the success path, and the documented benign already-claimed-today cooldown (`errorCode=120289`) returning nil, not an error |
| `sfsobject_fuzz_test.go` | Native Go fuzz targets `FuzzDecodeObject`/`FuzzReadPacket`, seeded with real encoded fixtures plus this repo's existing hostile-input edge cases (nesting/node-count bombs, negative counts, oversized frames), asserting only that the wire-format parsers never panic on malformed input; run for a bounded time in CI as defense-in-depth alongside the hand-written edge-case tests |
| `tools/reassemble_stream.py` | Reassembles one TCP stream from a pcap into `-decode-stream`-ready files — see [Capturing and decoding traffic](docs/capturing-and-decoding-traffic.mdx) |

## License

Apache License 2.0 -- see [LICENSE](LICENSE).
