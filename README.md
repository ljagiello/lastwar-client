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
  binding, resource collection across 13 confirmed building types (Farmland, Iron Mine, Gold Mine,
  Smelter, Material Workshop, Training Base, Oil Well, Drone Parts Workshop, Component Factory, and
  the four Season 6 Spore Factory tiers — 7 more types are wired in but still unconfirmed, see the
  building-type table in `docs/live-validation.mdx`), and a growing set of account-level
  automations: the "Armed Truck"/"Overlord" idle rewards, greeting city visitors, bulk-helping alliance
  members, claiming all mail and alliance gifts, donating to the alliance's currently-recommended
  tech, and both once-a-day VIP claims.
- **Confirmed live (previously an open question):** reconnecting into an *established* real
  account's live game state, with the code exactly as it ships today — i.e. with the `ta` analytics
  blob's device/anti-fraud sub-fields (`LwDeviceID`/`LwShumeiID`/`LwAirKey`) sent as the empty
  placeholders a round-13 security fix put in. This was the dossier's biggest open question (the
  original reconnect proof used `ta`'s *real* captured sub-fields, and it was unknown whether the
  placeholders would still be accepted). It's now settled: an unattended cron running this
  from-scratch Go client reconnected and collected real resources over multiple days (August 2026),
  and it was re-verified after a live zone-server migration. So the server does **not** require
  `ta`'s real device sub-fields for reconnect. Still open: the *minimal* required `ta` content
  hasn't been isolated, and a fully from-scratch login (no captured access token) hasn't been tried
  — see the next bullet. See `docs/live-validation.mdx` for the full methodology.
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
go build -o lastwar-client ./cmd/lastwar-client
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

# Collect resources from every confirmed building type, plus the Armed Truck/Overlord idle rewards,
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
GOOS=linux GOARCH=amd64 go build -o lastwar-client-linux-amd64 ./cmd/lastwar-client

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
- **`-log-level` controls the JSON log verbosity** (`debug`, `warn` — or its alias `warning` —, or `error`; default `info`) --
  handy for trimming a noisy cron log down to warnings/errors only, or turning on `debug` output
  while chasing down a problem run.

## Project layout

```text
cmd/lastwar-client/   thin CLI entry point (main -> app.Run)
internal/
  sfs/                SFS2X packet framing (incl. Zstd) + SFSObject binary codec, with redaction
  crypto/             GSL RSA-PKCS1v15 + AES-256-ECB-PKCS7 request/response envelope
  gsl/                GSL HTTP bootstrap (check-version, server list); depends on crypto, sfs
  session/            live game-server connection: dial, handshake, heartbeat, send/wait loop
  game/               Building/Visitor/Mail domain types + actions (buildings, mail, alliance, VIP, visitors)
  auth/               login, device identity, cross-server reconnect
  app/                CLI orchestration, interactive REPL, session config
  testutil/           test helpers shared across packages
docs/                 Mintlify protocol dossier -- start at docs/live-validation.mdx
tools/                pcap -> -decode-stream reassembly helper (see docs/capturing-and-decoding-traffic.mdx)
```

Packages are layered so the dependency graph stays acyclic: `sfs`/`crypto` are leaves, and each
layer only imports the ones below it (`session` -> `game` -> `auth` -> `app`).


## License

Apache License 2.0 -- see [LICENSE](LICENSE).
