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

**Use at your own risk: accounts can get banned.** Running a third-party client is likely against
the game's terms of service, and the server can refuse an account outright. In October 2026 a
research account was permanently banned
([discussion #53](https://github.com/ljagiello/lastwar-client/discussions/53)). Its client can send
marches, chat messages and `login.ext` answers, none of which this one does, and the ban's reason
code (703) is one the game doesn't explain, so what triggered it isn't known. Don't run this client
on an account you can't afford to lose. A banned or otherwise blocked account makes it exit with
code 4, see "Running unattended (cron)".

## Status

- **Fully working, live-confirmed:** GSL crypto, SFS2X packet framing (including Zstandard
  decompression), SFSObject codec, brand-new-guest-account login, email-verification account
  binding, resource collection across 14 confirmed building types (Farmland, Iron Mine, Gold Mine,
  Smelter, Material Workshop, Training Base, Oil Well, Drone Parts Workshop, Component Factory,
  Tactical Institute, and the four Season 6 Spore Factory tiers; the Crystal Factory and the
  Season 6 and Season 7 week-card buildings are wired in but unconfirmed), and a growing set of account-level
  automations: the "Armed Truck"/"Overlord" idle rewards, greeting city visitors, bulk-helping alliance
  members, claiming all mail and alliance gifts, donating to the alliance's currently-recommended
  tech, and both once-a-day VIP claims.
- **Confirmed live (previously an open question):** reconnecting into an *established* real
  account's live game state with the `ta` analytics blob's device/anti-fraud sub-fields
  (`lw_device_id`/`lw_shumei_id`/`lw_airKey`) sent as the empty placeholders a round-13 security
  fix put in. This was the dossier's biggest open question (the original reconnect proof used
  `ta`'s *real* captured sub-fields, and it was unknown whether the placeholders would still be
  accepted). It was settled in August 2026: an unattended cron running this from-scratch Go client
  reconnected and collected real resources over multiple days, and it was re-verified after a live
  zone-server migration. So the server did **not** require `ta`'s real device sub-fields for
  reconnect. Since the 1.0.370 follow-up the iOS `ta` no longer sends those placeholders: it
  carries the `Login`'s `deviceId` and `airKey` and the session config's `shumeiId`, as the 1.0.370
  app does, and a session config with a captured `ta` replays that `ta` with its build keys, the
  `Login`'s zone and those three ids taken from the run (static analysis only; one live
  config-copy test `Login` is needed before the cron uses it). Still open: the *minimal* required
  `ta` content hasn't been isolated. An Android from-scratch login is live-confirmed.
  See `docs/live-validation.mdx` for the full methodology.
- The earlier "reconnect blocked" / "init push never arrives" problems were never protocol-level
  gates, they were a token-identity mismatch (the client claimed Android while replaying an
  iOS-issued token) and an unimplemented Zstd decoder, respectively. A real server merge later
  exposed a third class of the same kind of bug: `serverInfo` zone-migration redirects were either
  unhandled or unreachable on both login paths. See `docs/live-validation.mdx` for the full
  root-cause writeups.
- **Not yet general-purpose:** the reconnect path needs a session config: one captured from a real
  client login (see below), or one `-own-device-session` writes for a device of its own, which
  refreshes its own tokens. `-cs-ios` on a guest or email login sends the iOS identity from the
  first GSL call through `account.login.new`. An iOS own-device bind, its `Login` and one token
  refresh worked in a third-party report on 2026-10-28 ([discussion
  #56](https://github.com/ljagiello/lastwar-client/discussions/56): one established account, one
  run); it hasn't been reproduced here.

- **Follows the 1.0.370 client (iOS build 918, Android 1911) and its data tables 40413.** The
  November 2026 audit of that build was fixed in place: login, GSL and transport details, the
  ShuMei fingerprint, every generated table, and the features' gates. A follow-up applied the
  user's decisions on it: the iOS `ta` is filled like the app's, `finished-timers` holds finished
  upgrades and research for their Alliance Duel day, and a read-only alliance chat reader feeds
  four chat features. Its fixes and the 66 opt-in features it added are static analysis only and
  haven't been sent live yet; see [docs/client-1-0-370.mdx](docs/client-1-0-370.mdx) for what
  changed and what to check on the first runs.

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
  "appVersion": "1.0.370",
  "versionCode": "918"
}
```

**Where these values come from:** capture a real login (e.g. `tcpdump` while the real app logs in,
since the SFS2X game socket is plain TCP with no TLS) and let `go run ./cmd/pcap -in login.pcap
-session-out ~/.lastwar_goclient_session.json` write this file from the captured `Login` (see
"Recovering" below, and `docs/capturing-and-decoding-traffic.mdx` for the manual methodology). The
access token is not single-use, but it *is* bound to the platform identity (`iosMode`) and build
(`appVersion`/`versionCode`, optional, defaulting to the built-in values) it was issued under, and
it will eventually need refreshing from a fresh capture. With `-cs-shumei-gen`, the client also
adds a `shumeiInstall` object: the per-install values its ShuMei fingerprint reports, rewritten
after each registration (see `docs/shumei.mdx`). Keep that object. Copying a config without
`shumeiInstall` over it makes the next registration report a different phone, and copying an
older one sends a stale `boxId`. A recapture with `pcap -session-out` (see "Recovering" below)
onto the existing session config keeps `shumeiInstall`, as it keeps every key it doesn't write,
so there is nothing to copy. Only a capture written to a new path lacks it: if the `deviceId`
didn't change, copy the old `shumeiInstall` into the new file. If the `deviceId` did change, the
kept `shumeiInstall` belongs to the old one, and the next registration warns and registers a
fresh install for the new `deviceId`. This matters only for configs without a `shumeiBoxId`:
`-cs-shumei-gen` never registers when one is set, and a captured config usually has one.

**The game server address is looked up, not typed in.** `ip` (a `|`-delimited gateway list) and
`port` are optional. The real client asks GSL `getserverlist.php` for its role's server on every
cold start, and since the 1.0.370 fixes this client does too:

- Every reconnect makes one GSL lookup (no `opt` field, the config's `deviceId`, `zone` and
  `gameUid`, and `platform=iOS` under `iosMode`) before the `Login`, unless the run just refreshed
  its tokens or the address was typed on the command line. With no `ip`/`port` saved, the lookup
  is the only way to find the server.
- The lookup fails open: an error, GSL 211/212 included, keeps the configured address and token.
  It dials the listed server and adopts any access or refresh token the reply carries. A reply
  that rotates the refresh token is saved to the config before the `Login`.
- When the listed server can't be reached, the `Login` is retried at the configured address. When
  a server refuses a token a lookup handed back (`E011`), the `Login` is retried there with the
  token the lookup replaced (the configured one, or the one this run's refresh issued), after any
  lookup. If no lookup has worked yet and every gateway refuses the connection, or the `Login`
  gets no reply, the client looks the server up once and retries there.
- The address it connected to is saved back into the config. On the first run after an update,
  GSL's gateway host names will likely replace the captured IPs.

`pcap -session-out` still writes the captured `ip`/`port`. Before the 1.0.370 fixes the lookup
ran only when needed and took only the address from the reply. Confirmed live on
2026-10-10 with a captured iOS session: the lookup returned the role's server and no tokens, and
configs with no address and with the pre-August port `17783` both logged in through it. `opt=fix`
is rejected for such a session with GSL code 212, which is why the lookup sends no `opt`.

**Recognizing an expired token, confirmed live:** every command starts failing with
`CROSS-SERVER LOGIN FAILED: ec=28 full={ep=[E011], ec=28}`, the connection succeeds, but login
itself is rejected, so nothing downstream even gets attempted. Don't confuse this with a single
flaky run: it was 100% reproducible across 16 consecutive scheduled runs (every 3 hours for ~42
hours) until the credentials were refreshed. For a captured config, fixing it needs a fresh capture,
same as initial setup.
In both real recurrences so far (2026-08-16 and 2026-10-02), `shumeiBoxId` had also changed, not just
`accessToken`, and the 2026-10-02 one happened server-side with no other login on that device.
The client now logs a dedicated "access token has been rotated" error for this case (exit code 2).
`ec=28` on its own says nothing: the reason is in `ep`. An `ep` that starts with `4;`, such as
`ep=[4;-9223372036847575810;13;703;<roleName>]`, is a ban, not a token problem (reported in
discussion #53, not seen here). A fresh capture can't fix it, so the client exits with code 4
instead, see `docs/auth.mdx`, "Reading a ban".

**A device of its own refreshes itself, confirmed live:** a config written by `-own-device-session`
holds the device's `refreshToken`. On `E011`, or once `accessTokenTime` is 30 days old, the client
trades it for a fresh access token through GSL `opt=refresh`, saves the pair to the config right
away, and logs in, so no capture is needed. See `docs/auth.mdx`, "A device of its own" and
"Refreshing the access token".

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
under, the `shumeiId` (the last `push.shumei.id` after the accepted `Login`, else the captured
`ta`'s `lw_shumei_id`), and the captured `ip`/`port` as a starting address, with mode 0600, and
prints only field lengths, never values. From an iOS capture it also records the `Login`'s device
fields (`phone_model`, `osVersion`, `mt`, `idfv`, `distinct_id`, `fromCountry`, `lang`,
`phone_screen`, `afuid`, `firebaseId`) and its `ta` under `device`; an iOS reconnect sends them
again, `fromCountry`/`lang` in the GSL form too, and replays the captured `ta` byte for byte with
`#app_version`, `lw_version`, `lw_res_version`, `lw_buildcode`, `lw_zone`, `lw_device_id`,
`lw_airKey` and `lw_shumei_id` taken from the run. Without the device fields the `Login` keeps the
placeholder `GoClient` model and the run warns to recapture; a captured device without a `ta`
(recorded before the 1.0.370 follow-up) warns too. Written onto an existing session config,
`-session-out` keeps every key it doesn't write (`features`, `duelPolicy`, `shumeiInstall` and any
other), drops `accessTokenTime` when the token changed and the refresh token when the `deviceId`
changed, and refuses a file that isn't a JSON object. So a recapture can go straight onto the live
config, with no hand merge. A run also saves the `shumeiId` a `push.shumei.id` brings into the
config, for its own `deviceId` only, and the next iOS `Login` sends it as `ta` `lw_shumei_id`, as
the app does. Recording the build matters: a token is bound to it, so once the real app updates, a
fresh token only works if the Login claims that same build. A config without these two fields
keeps claiming the build such configs always had, 1.0.344/786 on iOS or 1.0.364/1892 on Android,
and the run warns to add them. Fresh binds, `-own-device-session` and runs without a config claim
the current release, 1.0.370 (918 on iOS, 1911 on Android).

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
# free pulls, troop conversion, spending items or tickets) and are never turned on by default.
# A run goes in three phases, by name within each: claims that free capacity (finished-timers,
# secret-tasks-claim, and truck-reroll, so truck-deploy sends a truck rerolled to UR), then the
# actions, then the progress claims (daily-quests, arms-race-chests, battle-pass, quests,
# event-claims and the opt-in claims such as battle-pass-overflow; duel-gacha-draw after
# duel-gacha's pity claim), so those that re-read the server also claim what the run's own
# actions earned (quests and stage-quests read init, so they claim it on the next run).
# A feature that panics is logged with its stack and counted as failed; the others still run:
./lastwar-client -list-features
./lastwar-client -run daily-quests

# Alliance Duel gating (docs/alliance-duel.mdx). A feature can declare the duel score types it
# earns; with "hold" it runs only on a day whose duel entry lists one of them (-list-features
# shows duel:hold / duel:always / duel:-). Override per feature in the session config:
#   {"features": {"radar-claims": true}, "duelPolicy": {"radar-claims": "always"}}
# A duelPolicy entry for an unknown feature, or with a value other than "hold" or "always"
# (e.g. "Hold"), is warned about and ignored, so that feature keeps its default.
# -run of a held feature on a day that doesn't list its types is refused; -run-anyway runs it.
./lastwar-client -run radar-claims -run-anyway

# A duel or dig spend, supplies-search or free-research-finish that sees the diamond balance
# fall writes <session config>.tripwire.json. While that file exists every duel spend, the
# Treasure Hunt pickaxe digs, treasure-digs' brick opens, supplies-search and
# free-research-finish refuse to run, in later runs too (each run warns at startup; a -collect
# run exits 1). Free dig claims still run. Check the account, then remove the file (for -config,
# or the default session config) and exit:
./lastwar-client -clear-tripwire

# Stay connected and issue ad-hoc test commands without re-authenticating. NOTE: since
# -collect isn't passed here, the full building list still prints to stdout once at
# startup by default (this is true of every run that omits -collect, not just -interactive
# ones -- pass -collect to suppress it, or -list-buildings to make the print explicit).
# Only flat scalar params (strings/bools/numbers) are supported over this control FIFO --
# nested/array-shaped params (e.g. the "heroes" array docs/military-battle.mdx documents for
# StartArenaBattleMessage) can't be exercised this way today. Integers that fit 32 bits go out
# as SFS Int, larger ones as Long; a key suffix :i, :l, :d, :s or :b picks the type, e.g.
# {"uuid:l":42} (the command catalog names each param's type):
mkfifo /tmp/lw_cmd_pipe
./lastwar-client -interactive /tmp/lw_cmd_pipe &
echo 'building.production.collect {"uuid:l":123}' > /tmp/lw_cmd_pipe

# Override specific config fields for a one-off test (e.g. a different captured token). A typed
# -cs-at is not saved into its own device's session config, which keeps its own token:
./lastwar-client -collect -cs-at <a-different-access-token>

# Brand-new guest account instead (always works, no email or config needed):
./lastwar-client -list-buildings -no-config

# Bind a guest session to a real account via email verification (only needed once,
# to obtain a fresh config -- see docs/capturing-and-decoding-traffic.mdx for turning
# that into a session config):
mkfifo /tmp/lw_code_pipe
./lastwar-client -email you@example.com -code-pipe /tmp/lw_code_pipe &
echo 123456 > /tmp/lw_code_pipe

# Or give this client a device of its own in one run, no capture needed: bind a fresh device
# by email, trade its loginKey for its own token pair, and write a session config that
# refreshes itself (see docs/auth.mdx, "A device of its own"). The run logs the account's
# roles; add -server <number> to bind one other than the account's own pick:
LASTWAR_STATE_DIR=~/.lastwar_own_device ./lastwar-client -email you@example.com \
  -code-pipe /tmp/lw_code_pipe -own-device-session ~/own_device_session.json &
echo 123456 > /tmp/lw_code_pipe
LASTWAR_STATE_DIR=~/.lastwar_own_device ./lastwar-client -config ~/own_device_session.json -collect

# The same bind as iOS. -cs-ios makes check-version and the Login claim com.lastwar.ios,
# GSL send platform=iOS, and account.login.new send pf=AppStore. The session config
# is written with iosMode true. A third-party report says this iOS bind works
# (discussion #56); it hasn't been reproduced here. Use a fresh LASTWAR_STATE_DIR: a
# device that already holds a loginKey skips the email and trades that key instead:
LASTWAR_STATE_DIR=~/.lastwar_ios_device ./lastwar-client -cs-ios \
  -email you@example.com -code-pipe /tmp/lw_code_pipe \
  -own-device-session ~/ios_session.json &
echo 123456 > /tmp/lw_code_pipe
```

Device identity also persists across runs in `~/.lastwar_goclient_*` (deviceId, username, gameUid,
loginKey) independent of the session config, so repeated guest/email-flow runs present a consistent
device to the server. Delete those files to start fully fresh.

### Duel-aware features

These read today's Alliance Duel entry, or sit with the duel features they belong to;
`-list-features` prints the full set. All are off by default; `finished-timers` is one the
production config enables. The spending ones send only owned items or tickets, refuse any diamond
parameter, and stop for the rest of the process if a reply shows the diamond balance falling;
`duel-gacha-draw` spends only the gacha's own score balance. See
[docs/alliance-duel.mdx](docs/alliance-duel.mdx) for the wire details.

`finished-timers` and `free-research-finish` hold each completion inside the run rather than
through `Feature.Duel`, so `-list-features` shows `duel:-` for both and `duelPolicy` doesn't
apply to them. A held research keeps its Tech Center's queue idle until a day that scores
research power, up to four days after a Saturday finish, and a held HQ upgrade delays everything
gated on the HQ level.

| Feature | What it does |
|---|---|
| `finished-timers` | Enabled in the production config, not opt-in: collects ended timers for free (finished upgrades, repairs, research and healing queues, camp output). Since 1.0.370 it holds a finished upgrade until a day that scores building power (type 23: Base Expansion, Total Mobilization) and a finished research until one that scores research power (type 6: Age of Science, Total Mobilization). It finishes as before when there is no duel (no alliance, no open duel activity) and, with a Warn, when it can't read the duel. Repairs, healing and camp output are never held |
| `free-research-finish` | OPT-IN, 1.0.370: `queue.ccd.m.new {qUUID, itemIDs: ""}` on a running research whose remaining time is inside the free research time (init effect 30100, less 30 s). Held like `finished-timers`' research. The same bytes are also the client's diamond instant finish behind its diamond confirm (`UISpeedUpConfirmView.lua:131-135`, `ArmyManager.lua:204-210`), so only the research-queue check and the free-time window keep it free; a diamond drop in the reply stops it and sets the tripwire |
| `duel-speedups` | Applies owned speed-up items (`build.ccd.m.new`, `queue.ccd.m.new`, `building.camp.accel`, items-only form) to running jobs whose queue today's entry lists under score type 51 |
| `duel-speedups-plan` | Read-only: logs the speed-up calls `duel-speedups` would send |
| `duel-recruit-tickets` | Spends held hero or survivor recruit tickets (`lottery.hero.card` / `lottery.worker.card`, `useFree: 0`) on days whose entry lists recruit types 42 / 120. It keeps the tickets of a pool where the recruit window would first ask about the wish hero (no wish hero chosen, or the chosen one owned at max rank or max honor while another wish-list hero isn't) |
| `duel-troop-training` | Starts `building.camp.training` in idle Military Camps, at the highest unlocked tier and a count the food and iron in `init` cover, on Total Mobilization / Enemy Buster days. On Enemy Buster a batch starts only with a training speed-up item held and a later run left in the day. T11 trains in season 4 from day 64 on |
| `duel-gacha` / `-plan` | OPT-IN, 1.0.370: reads the duel score gacha (`al.duel.lottery.get.info`) and claims its full pity reward; never draws (see `duel-gacha-draw`). The plan only reads |
| `duel-gacha-draw` | OPT-IN, 1.0.370 (user carve-out): draws the duel score gacha one at a time (`al.duel.lottery.lottery`, `num` 1) while its own score balance pays 10,000,000 a draw, at most 50 a run, 1 s apart, and only on a Sunday (server time) or while an Alliance Duel runs, where the client shows its draw buttons. It doesn't read the init switch `alliance_duel` that gates the client's duel window. Spends only that score, never diamonds or items. Stops on any error code. Runs after `duel-gacha` |
| `secret-task-assist` | OPT-IN, 1.0.370: assists up to 5 allies' finished Secret Tasks a day from the alliance tab (`hero.dispatch.assist`, free); the helped ally sees it; never steals and never dispatches heroes. Scores no duel row, so it is not held |
| `secret-task-assist-plan` | Read-only, 1.0.370: logs the allies' finished Secret Tasks `secret-task-assist` would assist and today's assist count; never assists |
| `explorer-treasure-plan` | Read-only, 1.0.370: logs the held Explorer Treasure keys and key boxes; sends nothing |
| `radar-rescue-claims` | OPT-IN, 1.0.370, held for type 82: `receive.detect.event.reward` for finished rescue tasks whose rescued soldiers fit the stock |
| `supplies-search` | OPT-IN, 1.0.370: the one risk-free (level 1) step of a radar or building supplies search, then its reward; radar searches only on radar-scoring duel days, building searches every day |
| `secret-tasks-start` | Starts not-yet-started UR Secret Tasks (`hero.dispatch.start`) with idle heroes picked the way the client's Quick Join does; never refreshes a task |
| `secret-tasks-start-plan` | Read-only: logs which task it would start with which heroes |
| `radar-execute` | Runs the client's Quick Execute flows for march-free radar tasks (sampling, visitor, Help Teammates above a stamina reserve) and leaves them finished; the talk flow, which claims as it finishes, only on days whose entry lists type 82 |
| `radar-inventory` | Read-only: logs every radar task with its type and state, what `radar-execute` would do with it, and the bank figures |

### Firework and dig features

All are off by default. Every dig write passes a per-command key allowlist that refuses any `buy`, gold or diamond key.

| Feature | What it does |
|---|---|
| `fireworks-scan` | Read-only: lists the firework chests on alliance members' HQ tiles (`al.rank`, then `world.get.block`), with other players shown only as `member#<rank index>`. Since 1.0.370 the fireworks and treasure features share one scan per run: three blocks around each member HQ, read in viewport-shaped requests of at most 160 blocks, 0.5 s apart |
| `fireworks` | The same scan, then `get.fireworks.gift` for each eligible chest: under 120 minutes old, not full, from a current member; 1 s apart; stops at the daily cap |
| `alliance-treasures-plan` | Read-only: reads the world blocks around every member HQ (at least 30 tiles each way) and lists the radar treasures allies dug (the alliance chat's "treasure ... has been dug up!"), each with its verdict and its distance from the owner's HQ |
| `alliance-treasures` | The same scan, then `detect.event.claim.treasure` for each treasure that is dug, not expired, from your alliance, not yours, not full and not already claimed by you; 1 s apart; stops on any unexpected error |
| `fireworks-chat` | OPT-IN, 1.0.370: reads the alliance chat once a run (shared with the other chat features, never posts) and claims the chests its firework cards (posts 693, 695) point at: `find.fireworks.gift.world.point`, one `world.get.block` for the chest's block on your server, then the same rules and `get.fireworks.gift` as `fireworks`. Reaches the S0 alliance-boss chests on alliance buildings away from member HQs, which the HQ scan does not read. Skips chests `fireworks` already answered this run and stops when it hit the daily cap |
| `alliance-treasures-chat` | OPT-IN, 1.0.370: claims the treasures the chat's "treasure ... has been dug up!" cards (post 600) point at, wherever they are: `detect.event.get.treasure.claim.info {uuid, source 1, targetServer}`, one `world.get.block`, then the same rules and `detect.event.claim.treasure` as `alliance-treasures`. Only on your login server; skips cards whose lookup reply has `num` 0 or none (the client's `remainNum`; unverified) and treasures you already claimed |
| `treasure-digs` / `-plan` | Radar-ruin, city-ruin and Secret Vault dig boards: claims the free hammer when the board offers it, opens bricks with owned hammers only, and claims the chests |
| `treasure-hunt` / `-plan` | Treasure Hunt events (v1/v2): digs with owned pickaxes only and claims the tier and stored rewards |
| `season-dig`, `offseason-dig`, `alliance-boss-dig` / `-plan` | Alliance dig vaults: opens your one free stone and claims relic and personal rewards. `season-dig` covers the Season 3 and Season 7 vaults; `treasure-digs` also covers the S7 off-season vault and chest track |

### Alliance chat reader

Off by default and static-analysis-only. Since 1.0.370 the client has a read-only reader for the
alliance chat room, which runs on its own WebSocket, not the game socket (see
[docs/alliance-chat-mail.mdx](docs/alliance-chat-mail.mdx)). It sends only `room.joinMulti` and
`history.roomsv2`, never a message, like or share, and reads only the alliance room's newest page
and 3 s of pushes. One chat read per run serves every chat feature: `chat-scan`, `red-packets`,
`fireworks-chat` and `alliance-treasures-chat`. The chat server keeps one session per uid, so that
read drops the phone's chat connection (`another.login`), which doesn't reconnect by itself, the
way a run's game login drops the phone's game session.

| Feature | What it does |
|---|---|
| `chat-scan` | Read-only: logs every red packet, firework and treasure-dug card in the alliance room with its verdict; sends nothing on the game socket |
| `red-packets` | OPT-IN: opens the alliance room's red packets (post 611, `open.red.packet`) the game would let you open, the one expiring first first, 1 s apart, at most 10 a run; stops at the daily cap (`400600`) |

### Railway features

All three are off by default and static-analysis-only: their requests come from the 1.0.370 client and haven't been sent live yet. Before each truck send or truck attack `truck-deploy` and `railway-plunder` save the squad with `formation.save.template`, and before each train attack they fetch the train lineup with `alliance.train.formation`, as the client does. They skip a run while the city is in another warzone, while the station is still fogged, before its opening reward is taken (a `trainData` without `openReward` counts as taken, as in the game), and when the station doesn't report today's count. They ask for `train.data` only while the truck activity is open, and otherwise use the login's `trainData`, as the game does. A timeout or any refusal stops the run, except a target someone else just looted (`E400207`).

| Feature | What it does |
|---|---|
| `truck-deploy` | Sends your own parked trucks (`train.send`), **UR only**: quality 5 and the Reindeer Sleigh (10), never anything below. Each gets the strongest free defence squad. It stops at the daily send limit (4 plus effect 90103). Unused sends are lost at the reset, so it runs every day; a UR dispatch scores Alliance Duel type 98 on Tue and Sat |
| `truck-reroll` | OPT-IN, 1.0.370: runs before `truck-deploy` and uses the one free reroll (`train.change`) of each of your own parked N, R or SR trucks that could leave now, when `changeCountByGold` says it is free (`train_para` 9 is "0,1": only the first reroll of a truck). It keeps a truck that carries the "high goods" the game asks about (2 of item 2270000 or 500105, `train_para` 55) or whose goods can't be read, and never touches SSR, UR or the sleigh. Once per truck per run; it logs the new quality and never deploys, never spends item 1520001 or diamonds, never `train.batch.change` |
| `railway-plunder` | Plunders other players' trucks (`train.attack`) and other alliances' trains (`alliance.train.attack.kof`, or `alliance.train.attack` before the KOF mode). See the rules below |

`railway-plunder` drops a target that breaks any of these rules, before anything else:

- **Never your own server.** The target's `serverId` must differ from init `user.serverId` and from `user.crossFightSrcServerId`. Without a known `user.serverId`, it plunders nothing. The game doesn't enforce this: it only checks the server-sent `trainServers`, and your own server can be in that list.
- **Trucks: UR only**, the sleigh included.
- **Weaker than you.** The target's defensive power must be known and below the attacking power. A truck's power is its `marchInfo.power`. A train's is the sum of its defending teams, unknown if any team lacks a power.
  - The attacking power is your strongest attack squad for a truck. For a train it's the three teams of the lineup the server returns, or your three strongest attack squads when it returns no lineup at all; a lineup with an empty team, a hero in two teams or a repeated team buff leaves trains alone.
  - It's a lower bound on what the game shows: hero power plus the virtual soldiers' power from the game's own formula. It leaves out the tactical weapon, chip set, T11 and Overlord terms.
  - It's built only from fresh `hero.init.detail.get` stats. Without them, or with a hero missing, nothing is plundered.

After those come the game's own gates: 4 plunders a day shared by trucks and trains, 2 per truck and 1 per sleigh, the train cap and its protection time, matched servers, travelling targets only, and never your own alliance's.

### Claims added for 1.0.370

The 1.0.370 audit added these claims. Each is its own feature, off by default, and static-only:
its summary in `-list-features` ends "(static-only, not validated live)". None marches or sends a
chat message, and none spends anything except the user's carve-outs: `duel-gacha-draw` spends only
the duel gacha's own score balance, and `truck-reroll` uses only a truck's free reroll.
`secret-task-assist` is seen by the ally it helps. Each logs its first reply at Info. An error code
from one of its commands is logged and stops that claim; depending on the feature it also stops
the rest of that activity or building, or the feature for the run, while others (`city-soldiers`,
`radar-rescue-claims`, `supplies-search` among them) go on to their other claims. The duel,
firework, chat and railway ones are in the tables above: the follow-up added `free-research-finish`,
`duel-gacha-draw` and `secret-task-assist`, `fireworks-chat` and `alliance-treasures-chat`,
`chat-scan` and `red-packets`, and `truck-reroll`.

| Area | Features |
|---|---|
| City | `city-soldiers` (count-battle and Civilization Spark soldiers), `captive-visitors` (Season 7 Battle Plan captives, type 40), `cockatrice-visitors` (type 13), `hero-countdown-reward`, `city-garbage` |
| Dailies and mail | `flower-train-cheer-boxes`, `hero-wish-claim` (a full wish pity's chosen hero), `mail-single-claims-plan` (read-only: logs the type-112 mail Claim All leaves out) |
| Alliance | `base-bubbles` (Thanksgiving on members' HQs, Valentine on your own; the base's owner may see the claim), `city-first-occupation`, `s0-alliance-boss`, `alliance-star-emoji`, `zone-mobilization-tasks` |
| Events | `sunrise-foundation`, `gift-box-free-draw` (the free single draw only), `gift-box-score-boxes`, `hero-exchange-reward`, `world-boss-kill-rewards`, `food-event-boxes` / `-plan`, `banquet-v2-claims` / `-plan`, `bounty-hunter-milestones`, `bounty-hunter-chests`, `event-milestone-boxes` (Lucky Roll, Blue Store, slot machine), `decoration-gacha-boxes` |
| More events | `tactical-card-box-points`, `tactical-card-collection`, `offseason-tasks`, `survivor-pack` (free half only), `doomsday-quests`, `parkour-tasks`, `front-break-tasks`, `leading-quest-tasks`, `hero-trial-stage`, `sand-worm-tasks`, `crazy-rock-tasks`, `letters`, `sky-battle-boxes`, `newbie-arena-rewards`, `birthday-set-reward`, `mastery-newbie-reward`, `counter-attack-rounds`, `theme-prenotice` |
| Season | `season-achievements`, `military-rank-up`, `season-trends`, `season-outpost-boxes`, `cross-throne-rewards`, `bloody-night-tasks`, `jungle-trial-rewards`, `gold-tree-prayer` |

Two features that the production config enables changed in a way to check on the first runs:
`season-dailies` now sends the power-milestone pair only in a Desert (type 1) season, as the
client does, so in any other season the achievement grades are left to `season-achievements`;
and `quests` leaves the Season 7 Lockhart quest 2400008 to `stage-quests`. Both of those are off
by default and not validated live, so until `season-achievements` and `stage-quests` are
validated and enabled, nothing claims the non-Desert achievement grades or 2400008. See
docs/client-1-0-370.mdx for the full list.

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
  at all. GSL codes 211/212 ("re-auth needed") also exit `2`, and so does a
  `push.account.device.del` that removes this run's device from the account: bind it again or
  recapture. A blocked account, role, device or IP exits `4` instead, see below. A `Login` refused
  because the season's server window is closed (`E015`/`E016`) exits `1`: the next run may get in.
- **Exit code 3 means the server ended the session on purpose.** `push.user.off` (the account
  logged in on another device), `push.server.stop` (maintenance), `init.error`,
  `push.user.gm.off` (a GM kick) and `push.refund.restart` stop the run without reconnecting, as
  the real client does. Nothing needs fixing: the next scheduled run
  starts a fresh session. Playing on another device during a cron run is the usual cause.
- **Exit code 4 means the server refused the account, role, device or IP.** A ban, an IP or
  device blacklist, or an account or role deletion state (the numeric `ep` codes in
  `docs/auth.mdx`, "Login rejection codes"). A fresh capture or a token refresh can't fix it, so
  don't recapture: stop the cron until it's resolved. The log's error line names the code, and for
  a ban, whether it is permanent and its reason code. For a ban or a deletion, the real app on that
  account shows why.
- **Exit code 5 means the server demands a forced update.** Check-version or the `Login` reply
  said `updateType` 2: check-version stops the run before the GSL lookup, a `Login` reply before
  `init`, as the real client does. Move the session config's `appVersion`/`versionCode` to the
  current release (recapture with `pcap -session-out`).
- **Whatever wraps the cron must know these codes.** Since the 1.0.370 fixes a run can exit `5`
  (forced update: stop and update the build, don't retry), `2` on a `push.account.device.del`
  for this device, `1` on `E015`/`E016` (it used to be `2`), and `3` on `push.user.gm.off` and
  `push.refund.restart`. A wrapper that treats every non-zero exit alike, or only `2` as "recapture",
  needs updating before the cron runs this build.
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

Created by [Lukasz Jagiello](https://lukasz.jagiello.org). Write-up: [lukasz.jagiello.org/projects/lastwar-client](https://lukasz.jagiello.org/projects/lastwar-client).
