# lastwar-client

A from-scratch Go reimplementation of the *Last War: Survival Game* (`com.fun.lastwar.gp`) mobile
client's network layer: GSL RSA+AES bootstrap crypto, SFS2X wire protocol, and SFSObject binary
codec, enough to log in and drive a real game session over TCP without the Unity client.

Built by reverse-engineering the decompiled APK; the full protocol dossier is published at
**[lastwar.tech](https://lastwar.tech)** (source in [`docs/`](docs/)), covering the wire format,
crypto, the ~3,178-command catalog, entity IDs, and a running log of what's confirmed live against
real production servers versus what's still static analysis. Start with
[live-validation](https://lastwar.tech/live-validation) ([`docs/live-validation.mdx`](docs/live-validation.mdx))
for the current confirmed-vs-unconfirmed picture. Preview the docs locally with `mint dev` from inside
`docs/`.

## Status

- **Fully working, live-confirmed:** GSL crypto, SFS2X packet framing (including Zstandard
  decompression), SFSObject codec, brand-new-guest-account login, email-verification account
  binding, resource collection across 14 confirmed building types (Farmland, Iron Mine, Gold Mine,
  Smelter, Material Workshop, Training Base, Oil Well, Drone Parts Workshop, Component Factory,
  Tactical Institute, and the four Season 6 Spore Factory tiers; the 1.0.364 Crystal Factory and
  Season 6 week-card building are wired in but unconfirmed), and a growing set of account-level
  automations: the "Armed Truck"/"Overlord" idle rewards, greeting city visitors, bulk-helping alliance
  members, claiming all mail and alliance gifts, donating to the alliance's currently-recommended
  tech, and both once-a-day VIP claims.
- **Confirmed live (previously an open question):** reconnecting into an *established* real
  account's live game state, with the code exactly as it ships today, i.e. with the `ta` analytics
  blob's device/anti-fraud sub-fields (`LwDeviceID`/`LwShumeiID`/`LwAirKey`) sent as the empty
  placeholders a round-13 security fix put in. This was the dossier's biggest open question (the
  original reconnect proof used `ta`'s *real* captured sub-fields, and it was unknown whether the
  placeholders would still be accepted). It's now settled: an unattended cron running this
  from-scratch Go client reconnected and collected real resources over multiple days (August 2026),
  and it was re-verified after a live zone-server migration. So the server does **not** require
  `ta`'s real device sub-fields for reconnect. Still open: the *minimal* required `ta` content
  hasn't been isolated, and a fully from-scratch login (no captured access token) hasn't been tried
, see the next bullet. See `docs/live-validation.mdx` for the full methodology.
- The earlier "reconnect blocked" / "init push never arrives" problems were never protocol-level
  gates, they were a token-identity mismatch (the client claimed Android while replaying an
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
anything that reads as a cheating/exploit guide rather than protocol documentation, see
[`docs/AGENTS.md`](docs/AGENTS.md) for the full boundary.

## Build

```
go build -o lastwar-client ./cmd/lastwar-client
go test ./...
```

## Session config (recommended, avoids passing every `-cs-*` flag each run)

Reconnecting into an established account needs several values that can only come from a real
client's own login (device ID, access token, ShuMei fingerprint, ...). Rather than typing them on
the command line every time, put them in a JSON file:

```
cp config.example.json ~/.lastwar_goclient_session.json
chmod 600 ~/.lastwar_goclient_session.json
# then edit it with your own real values
```

`~/.lastwar_goclient_session.json` is auto-loaded on every run if present, no flag needed. To use
a different file, pass `-config /path/to/file.json`. Individual `-cs-*` flags still override
whatever the config file says, for one-off tests.

```json
{
  "zone": "your-real-zone-e.g.-APS1234",
  "gameUid": "your-real-composite-gameUid",
  "deviceId": "your-real-device-id_n3d",
  "shumeiBoxId": "your-real-shumei-fingerprint-token",
  "accessToken": "your-real-access-token-from-a-captured-login",
  "iosMode": true,
  "appVersion": "1.0.344",
  "versionCode": "786"
}
```

**Where these values come from:** capture a real login (e.g. `tcpdump` while the real app logs in,
since the SFS2X game socket is plain TCP with no TLS) and let `go run ./cmd/pcap -in login.pcap
-session-out ~/.lastwar_goclient_session.json` write this file from the captured `Login` (see
"Recovering" below, and `docs/capturing-and-decoding-traffic.mdx` for the manual methodology). The
access token is not single-use, but it *is* bound to the platform identity (`iosMode`) and build
(`appVersion`/`versionCode`, optional, defaulting to the built-in values) it was issued under, and
it will eventually need refreshing from a fresh capture.

**The game server address is looked up, not typed in.** `ip` (a `|`-delimited gateway list) and
`port` are optional. The real client asks GSL `getserverlist.php` for its role's server on every
cold start, and this client does the same when it needs to:

- With no `ip`/`port` in the config or on the command line, it looks the role's server up before
  dialing (`opt=fix` with the config's `deviceId`, `zone` and `gameUid`, and `platform=iOS` under
  `iosMode`).
- With an address saved, it dials it directly and makes no GSL call. If every gateway refuses the
  connection, or the `Login` gets no reply (the August 2026 port move looked like this), it looks
  the server up once and retries there. An `E011` or other rejection never triggers a lookup.
- Either way, the address it connected to is saved back into the config, so later runs dial it
  directly.

`pcap -session-out` still writes the captured `ip`/`port`, which saves the first lookup. The lookup
takes only the address from the reply and keeps the config's `accessToken`. It is
static-analysis-only: whether GSL honors `opt=fix` for a session the iOS app issued, and whether that
call issues or rotates tokens, hasn't been tested live. If the lookup logs that the reply carries a
different access token and the `Login` then fails with `E011`, recapture the session.

**Recognizing an expired token, confirmed live:** every command starts failing with
`CROSS-SERVER LOGIN FAILED: ec=28 full={ep=[E011], ec=28}`, the connection succeeds, but login
itself is rejected, so nothing downstream even gets attempted. Don't confuse this with a single
flaky run: it was 100% reproducible across 16 consecutive scheduled runs (every 3 hours for ~42
hours) until the credentials were refreshed. Fixing it needs a fresh capture, same as initial setup.
In both real recurrences so far (2026-08-16 and 2026-10-02), `shumeiBoxId` had also changed, not just
`accessToken`, and the 2026-10-02 one happened server-side with no other login on that device.
The client now logs a dedicated "access token has been rotated" error for this case (exit code 2).

**Recovering, one command after the capture:** start a capture (no `sudo` needed if Wireshark's
ChmodBPF is installed; use the real interface, `-i any` needs root on macOS), cold-start the real
app until its main screen loads, quit it, then let `pcap` write the session config:

```
tcpdump -i en0 -w login.pcap 'tcp and not port 443 and not port 22'
go run ./cmd/pcap -in login.pcap -session-out ~/.lastwar_goclient_session.json
```

`-session-out` finds the Login the server *accepted* (the real app first races the zone port on
several gateways with `a=29` probes; those are skipped), writes `zone`/`gameUid`/`deviceId`/
`shumeiBoxId`/`accessToken`/`iosMode` plus the `appVersion`/`versionCode` the token was issued
under, and the captured `ip`/`port` as a starting address, with mode 0600, and prints only field
lengths, never values. Recording the build
matters: a token is bound to it, so once the real app updates, a fresh token only works if the
Login claims that same build (without these two fields, iOS mode falls back to the 1.0.344/786
build the original capture showed).

**This file contains live credentials for a real account, keep it out of version control** (it's
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

# Optional features (daily quests, free chests, event claims, ...): every one is off until it
# has been validated live. List them with their state, run one once to validate it, then turn
# it on in the session config's "features" map, e.g. {"features": {"daily-quests": true}}.
# Features marked OPT-IN change something visible or a player choice (likes, help requests,
# free pulls, troop conversion, spending items or tickets) and are never turned on by default:
./lastwar-client -list-features
./lastwar-client -run daily-quests

# Alliance Duel gating (docs/alliance-duel.mdx). A feature can declare the duel score types it
# earns; with "hold" it runs only on a day whose duel entry lists one of them (-list-features
# shows duel:hold / duel:always / duel:-). Override per feature in the session config:
#   {"features": {"radar-claims": true}, "duelPolicy": {"radar-claims": "always"}}
# -run of a held feature on a day that doesn't list its types is refused; -run-anyway runs it.
./lastwar-client -run radar-claims -run-anyway

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

### Duel-aware features

These read today's Alliance Duel entry; `-list-features` prints the full set. All are off by default.
The spending ones send only owned items or tickets, refuse any diamond parameter, and stop for the
rest of the process if a reply shows the diamond balance falling. See
[docs/alliance-duel.mdx](docs/alliance-duel.mdx) for the wire details.

| Feature | What it does |
|---|---|
| `duel-speedups` | Applies owned speed-up items (`build.ccd.m.new`, `queue.ccd.m.new`, `building.camp.accel`, items-only form) to running jobs whose queue today's entry lists under score type 51 |
| `duel-speedups-plan` | Read-only: logs the speed-up calls `duel-speedups` would send |
| `duel-recruit-tickets` | Spends held hero or survivor recruit tickets (`lottery.hero.card` / `lottery.worker.card`, `useFree: 0`) on days whose entry lists recruit types 42 / 120 |
| `duel-troop-training` | Starts `building.camp.training` in idle Military Camps, at the highest unlocked tier and a count the food and iron in `init` cover, on Total Mobilization / Enemy Buster days |
| `secret-tasks-start` | Starts not-yet-started UR Secret Tasks (`hero.dispatch.start`) with idle heroes picked the way the client's Quick Join does; never refreshes a task |
| `secret-tasks-start-plan` | Read-only: logs which task it would start with which heroes |
| `radar-execute` | Runs the client's Quick Execute flows for march-free radar tasks (sampling, visitor, Help Teammates above a stamina reserve) and leaves them finished; the talk flow, which claims as it finishes, only on days whose entry lists type 82 |
| `radar-inventory` | Read-only: logs every radar task with its type and state, what `radar-execute` would do with it, and the bank figures |

### Firework and dig features

All are off by default. Every dig write passes a per-command key allowlist that refuses any `buy`, gold or diamond key.

| Feature | What it does |
|---|---|
| `fireworks-scan` | Read-only: lists the firework chests on alliance members' HQ tiles (`al.rank`, then `world.get.block`), with other players shown only as `member#<rank index>` |
| `fireworks` | The same scan, then `get.fireworks.gift` for each eligible chest: under 120 minutes old, not full, from a current member; 1 s apart; stops at the daily cap |
| `alliance-treasures-plan` | Read-only: reads the world blocks around every member HQ (at least 30 tiles each way) and lists the radar treasures allies dug (the alliance chat's "treasure ... has been dug up!"), each with its verdict and its distance from the owner's HQ |
| `alliance-treasures` | The same scan, then `detect.event.claim.treasure` for each treasure that is dug, not expired, from your alliance, not yours, not full and not already claimed by you; 1 s apart; stops on any unexpected error |
| `treasure-digs` / `-plan` | Radar-ruin, city-ruin and Secret Vault dig boards: claims the free hammer when the board offers it, opens bricks with owned hammers only, and claims the chests |
| `treasure-hunt` / `-plan` | Treasure Hunt events (v1/v2): digs with owned pickaxes only and claims the tier and stored rewards |
| `season-dig`, `offseason-dig`, `alliance-boss-dig` / `-plan` | Alliance dig vaults: opens your one free stone and claims relic and personal rewards |

Every run with a feature enabled logs one `alliance duel` line (theme, score, next chest, day end),
and every `push.act.score.obtain` the server sends is logged as `activity score obtained` with the
request sent just before it.

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

The server day, and with it the Alliance Duel day, rolls over at 02:00 UTC. Features gated on today's
duel entry see the new day from the first scheduled run after that.

Two things worth checking after setup, not just once but as ongoing habits:

- **Actually confirm cron itself fires the job**, not just that the binary runs when you invoke it
  manually over SSH, those aren't the same test. `crontab -l` accepting the entry doesn't prove
  the daemon is running or the schedule is right. Add a one-off entry a couple of minutes out,
  wait for it, and check both the log file *and* cron's own record of running it
  (`grep CRON /var/log/syslog` on Debian/Ubuntu) before trusting the real schedule.
- **The log file has no rotation** in the example above, it just appends forever. At 8 runs/day
  it takes a long time to matter, but for a long-lived deployment either cap it with `logrotate`
  or watch its size periodically.
- **Check the log's error rate periodically, not just whether the process is still running.** A
  cron job can "work" (exit 0, get invoked on schedule) while every run inside it is failing at
  the login step, see the token-expiry note in "Session config" above. Grepping the log for
  `"level":"ERROR"` and checking *when* errors started (not just whether any exist) is what
  actually catches this, see `docs/live-validation.mdx`'s serverInfo-redirect section for a
  real example of a failure that looked identical on every single run once it started.
- **Exit code 2 means the session itself is stale, not a transient blip.** Login/auth failures
  (both the plain-login and cross-server-reconnect paths) exit `2` specifically, distinct from
  the generic exit `1` used for other failures -- a cron wrapper can check `$?` directly and
  know to recapture a fresh session (see "Session config" above) without needing to grep the log
  at all. GSL codes 211/212 ("re-auth needed") also exit `2`.
- **Exit code 3 means the server ended the session on purpose.** `push.user.off` (the account
  logged in on another device), `push.server.stop` (maintenance) and `init.error` stop the run
  without reconnecting, as the real client does. Nothing needs fixing: the next scheduled run
  starts a fresh session. Playing on another device during a cron run is the usual cause.
- **`-log-level` controls the JSON log verbosity** (`debug`, `warn`, or its alias `warning`, or `error`; default `info`) --
  handy for trimming a noisy cron log down to warnings/errors only, or turning on `debug` output
  while chasing down a problem run.

## Project layout

```text
cmd/
  lastwar-client/     thin CLI entry point (main -> app.Run)
  pcap/               decode a capture: list TCP conversations, reassemble + decode a stream
internal/
  sfs/                SFS2X packet framing (incl. Zstd) + SFSObject binary codec, with redaction
  crypto/             GSL RSA-PKCS1v15 + AES-256-ECB-PKCS7 request/response envelope
  gsl/                GSL HTTP bootstrap (check-version, server list); depends on crypto, sfs
  session/            live game-server connection: dial, handshake, heartbeat, send/wait loop
  game/               Building/Visitor/Mail domain types + actions (buildings, mail, alliance, VIP, visitors)
  auth/               login, device identity, cross-server reconnect
  app/                CLI orchestration, interactive REPL, session config
  pcap/               pure-Go pcap + pcapng reader and TCP stream reassembler (used by cmd/pcap)
  testutil/           test helpers shared across packages
docs/                 Mintlify protocol dossier -- start at docs/live-validation.mdx
```

Packages are layered so the dependency graph stays acyclic: `sfs`/`crypto` are leaves, and each
layer only imports the ones below it (`session` -> `game` -> `auth` -> `app`).


## License

Apache License 2.0 -- see [LICENSE](LICENSE).
