# Handoff — make `shumeiBoxId` generation return a real `smid`

> **RESOLVED 2026-10-11.** The blocker described below is done: `internal/shumei`
> now reproduces the native `w1` protection (16-char nonce `r`, `key = MD5(r)`,
> raw-deflate + AES-256-CBC of the `aNN` fingerprint, RSA-OAEP of `r` as `ep`),
> and `BoxID()` returns `"B"` + a real `smid` (live-tested, `code:1100`). The
> remaining text is kept as the research record of how the scheme was recovered.

> For the next agent. Everything below is recovered/verified state; do **not** re-derive it.
> Goal: change `internal/shumei` so `BoxID()` POSTs a profile that the ShuMei backend
> answers with `{"code":1100,"detail":{"deviceId":"<smid>"}}` instead of `{"code":1902}`.

---

## TL;DR — current state

- `shumeiBoxId` = the ShuMei (数美) SDK's `smid`, assigned by `POST <url>/deviceprofile/v4`.
- The **easy half is done and correct**: config constants, endpoint, response shape, RSA-OAEP
  primitive, Go package `internal/shumei`, `-cs-shumei-gen` flag.
- The **hard half is missing**: the device-profile body is built by the SDK's device-data
  collector (~150 `aNN` fields from `SubCollector`/`AbsDetector` + the OLLVM-flattened native
  `w1`), then protected. The server answers `1902` to anything it can't use: a bogus
  envelope, a bare `{"organization":...}`, and the current `BoxID` body all get it. So `1902`
  doesn't mean "partial profile accepted".
- Live-verified 2026-10-10: empty body → `9101 "invalid organization"`; correct organization →
  `1902`; full minimal `BoxID` profile → `1902`. The `shumeiBoxId` in the operator's session
  config is 89 chars: `"B"` + the 88-char server `deviceId` (`getDeviceId()` adds the `"B"`).
  `BoxID()` returns the bare `deviceId`, so even a `1100` would give the wrong Login value.
- Two concrete bugs already fixed: `sdkver` is `"3.14.3"` (not `"3.14.3_build1"`); the crypto is
  hybrid RSA-OAEP + AES-256-CBC (not flat RSA-OAEP).

## Where artifacts live

| Artifact | Path |
|---|---|
| The `.so` being reversed | `/Users/lcf/Downloads/foo/split_config.arm64_v8a/lib/arm64-v8a/libsmsdk.so` |
| Decrypted `.data` (all 514 strings) | `/Users/lcf/Downloads/foo/analysis/decoded_strings.txt` |
| Raw decrypted `.data` bytes | `/Users/lcf/Downloads/foo/analysis/decoded_data.bin` |
| Ghidra decompilation (335 fns) | `/Users/lcf/Downloads/foo/analysis/decompiled.c` |
| JNI helper decompilations | `/Users/lcf/Downloads/foo/analysis/helpers.c` |
| `.so` with strings patched in | `/Users/lcf/Downloads/foo/analysis/libsmsdk_patched.so` |
| jadx Java sources (ishumei + sdkmanager, 141 files) | `/Users/lcf/Downloads/foo/analysis/jadx/` |
| iShumei public key PEM / cert DER / C header | `/Users/lcf/Downloads/foo/c_reimpl/` |
| Clean-room C reimpl (reference) | `/Users/lcf/Downloads/foo/c_reimpl/smsdk.c`, `deobfuscated.c`, `ishumei_key.h` |

Tools available on the dev Mac: `ghidra` (`/opt/homebrew/Cellar/ghidra/12.1.4/libexec`,
`analyzeHeadless`; use `pyghidra` from pip), `jadx` (`/opt/homebrew/bin/jadx`), `rizin`/`radare2`,
`angr`+`unicorn`+`capstone`+`pyelftools` (pip).

## Byte-exact facts (do not re-verify)

### Game config (`com.sdkmanager.SmSdkManager`)
- `organization = "9dNCYkjHeFMYZMOjxgX8"`, `appId = "default"`
- `publicKey` = self-signed X.509 cert `CN=e.ishumei.com, OU=SM, C=CN` (2048-bit RSA)
- `area = AREA_FJNY` (`"fjny"` → numeric `"011"`), `usingHttps = true`
- `url = https://shumei-api.lastwargame.com:19091/deviceprofile/v4`
- `confUrl = https://shumei-api.lastwargame.com:19091/v3/cloudconf`

### Endpoint / transport
- `POST /deviceprofile/v4`, header `Content-Type: application/octet-stream`, `Connection: Close`.
- The first request's body is the first of the two JSON objects native `w1` returns, sent as-is
  (`l11l111lll` → `l111l1111llIl.l1111l111111Il(flags, false)` → UTF-8 bytes). Java adds nothing.
- `"retry":1` is added **only** by `l11l11l1l1Il`, the background resend that the error handler
  starts for codes other than `1902`. The first request has no `retry` field.
- SDK built-in datasites are region-pinned on `fengkongcloud.com` (`fp-it`, `fp-sa-it`,
  `fp-na-it-acc`, + `api-fp-retry-*`); the game overrides them with the proxy above.

### Response
- `{"code":<int>,"detail":{"deviceId","oldDeviceId","sid","c","t"},"requestId"}` (classes
  `l1l11I1l`, `l11l111llI1l`).
- Codes: `1100` success, `1902` the server's generic "can't use this" reply (live: any body it
  can't decode gets it), `1903` hard error. `detail.deviceId` = the
  smid. Success handler: `l11l111lll.l1111l111111Il(l11l111llI1l)`.

### Profile field names (byte-exact, class `l111l1111lI1l`)
`organization, os, sdkver, smid, appId, encode, data, appname, tn, ep, sessionId, wevent, md5,
enc, pri, sid, bb` plus `a1`…`a174` device-data slots.

(Resolved: this class is the SDK's shared name table, not one request's body. `sdkver`, `md5`,
`pri` and `bb` belong to the **cloudconf** request (`l11l111l1lll.java`), and `smid`, `appname`,
`sessionId` and `wevent` to the **v3** event flow (`l1l1l111Il.java`). No decompiled Java builder
references `enc` or `sid`. The deviceprofile body is only `organization, os, appId, encode,
compress, data, tn, ep`, and the `a1`…`a174` slots live inside the protected `data`. See
`docs/shumei.mdx`.)

### Values
- `os = "android"`, `sdkver = "3.14.3"` (both from `l11l11lI1lll`). `getSDKVersion()` returns
  `"3.14.3_build1"` but that is a separate public API, **not** the `sdkver` field.
- Native `w1` writes `encode=2`, `compress=3` (numeric doubles), plus string fields
  organization/os/appId/data/tn/ep (Ghidra decomp of `FUN_0010f8d0`).

### Cryptography (class `l11l11l1lIl`, `l11l111l1lll`)
- RSA: `RSA/ECB/OAEPWITHSHA-256ANDMGF1PADDING`, i.e. RSA-OAEP with SHA-256 digest + MGF1-SHA256
  and **empty label** (`OAEPParameterSpec("SHA-256","MGF1",MGF1ParameterSpec.SHA256,
  PSource.PSpecified.DEFAULT)`). Go: `rsa.EncryptOAEP(sha256.New(), rand, pub, msg, nil)`.
- AES: `KeyGenerator("AES").init(256)` random key; `AES/CBC/PKCS5Padding` with IV
  `"0102030405060708"` (16 ASCII bytes).
- Hybrid (observed in the **cloudconf** request builder `l11l111l1lll`): the request `data`
  sub-object is `{os, sdkver, md5, enc:1, bb, sid, pri}` where `pri = rsaOaep(cert, aesKey)`;
  the AES key encrypts/decrypts the payload. The deviceprofile request shape is *likely* built
  the same way but must be traced in `l111l1111llIl` (see "What remains").

## Native facts (libsmsdk.so)

- `JNI_OnLoad @ 0xa79c`: FindClass `com/ishumei/smantifraud/dfp/SMSDK`, `RegisterNatives` of 9
  methods, reads `Build.VERSION.SDK_INT`.
- `JNINativeMethod` table @ `0xc6ec8` (9 entries):
  `w1` `(Context,String×7,long,String,List)→String` 0xa07c; `x4`/`x6` `(Str,Str)→Str` 0xa2d8/0xa2cc;
  `w3` `(Context,String×4)→Str` 0xa2b4; `ma` `()→Z` 0xa2e4; `ma2` `()→I` 0xa448; `d` `()→V` 0xa73c;
  `native_u2` `(Str)→J` 0xa764; `u3` `(Str)→V` 0xa5b4.
- Native crypto is done **via JNI to Java** (decoded strings include
  `RSA/ECB/OAEPWITHSHA-256ANDMGF1PADDING`, `getPublicKey`, `X.509`, `CertificateFactory`,
  `OAEPParameterSpec`, `MGF1`, `PSource$PSpecified`, `doFinal`, `encodeToString`, `Base64`,
  `SHA-256`, `deflate`).
- Native `w1` (`FUN_0010f8d0`) returns `{metadata}{encrypted_data}` — two JSON objects joined at
  `"}{"`; the Java `l111l1111llIl` splits on `indexOf("}{")+1` and stores each half.
- Native `x4` (`FUN_00117098`) parses a JSON map, `map.set(m,"4050607080102030",value)` (via
  `FUN_0019c5d0`), reserializes. So `"4050607080102030"` is a **map key** in the native payload,
  not an AES key (the `"01020304050607084050607080102030"` string reserved from `strings` is two
  adjacent constants: `"0102030405060708"` IV + `"4050607080102030"` key).

## Key Java class roles (in `analysis/jadx/ishumei/smantifraud/`)

| Class | Role |
|---|---|
| `SMSDK.java` | JNI surface, `System.loadLibrary("smsdk")` |
| `SmAntiFraud.java` | public API, `SmOption`, `getSDKVersion()` |
| `l11l11lI1lll.java` | version/os/host/path constants, region→code map |
| `l111l1111lI1l.java` | **all** profile field-name constants + 174 a-fields |
| `l1l11I1l.java` | response code + `detail`/`requestId` key constants |
| `l11l111llI1l.java` | response detail object (deviceId/sid/c/t) |
| `l11l11l1lIl.java` | crypto: RSA-OAEP, AES-256-CBC, KeyGenerator |
| `l11l111l1lll.java` | cloudconf request builder (hybrid), response decryptor |
| `l11l11l1l1Il.java` | background resend only (adds `retry`:1); not the first request |
| `l11l111lllIl.java` | the first device-profile request (body = `w1`'s first JSON object) |
| `l111l11I1IIIl.java` | `getDeviceId()` backing: `"B"` + server smid, else a `"D"` local fallback |
| `l11l111lll.java` | device-registration orchestrator (calls the builder, stores smid) |
| `l111l1111llIl.java` | **HEAVILY obfuscated** device-data collector; calls `SMSDK.v1`; body builder `l1111l111111Il(int flags, boolean)` |
| `l1l1l111Il.java` | `v3` flow: `{org,smid,appId,appname,sessionId,wevent}` → `SMSDK.v3` |

## What remains (the actual blocker)

The device-profile body is `l111l1111llIl.l111l11111lIl().l1111l111111Il(flags,false)` with
`flags = (no-cached-smid?2:0) | (needUsingMD5?1:0)`. That method mutates a per-device map through
~150 `l11l1111liil.lXXX(<getter>)` calls (the `SubCollector`/`AbsDetector` output — Android props,
`/proc`/mounts, app list, keystore attestation, etc.), then runs `SMSDK.v1` (native `w1`) to
protect it. `SMSDK.v1`'s output (`{metadata}{encrypted}`) is what the body carries.

To mint a real `smid`, the next agent needs to (in this order):

1. **Enumerate the 174 `aNN` fields** — they are byte-exact in `l111l1111lI1l.java`; map each to
   the collector getter that fills it by reading `l111l1111llIl.java`'s `l1111l111111Il(int,
   boolean)` and the `AbsDetector`/`SubCollector`/`l11l1111liil` classes. Produce a
   field-name → value-formula table (most are `__system_property_get(key)` or a file read).
2. **Request body shape: resolved, it's native.** Java doesn't assemble the body. It POSTs the
   first JSON object `w1` returns, unchanged. The nested `data`/`pri`/`sid`/`bb`/`enc` shape
   belongs to the **cloudconf** request (`l11l111l1lll`), not to deviceprofile.
3. **Reconstruct the native `w1` protection exactly** — the payload it protects, and whether it is
   RSA-OAEP-only or RSA-OAEP+AES; note `x4`'s `"4050607080102030"` map key.
4. **Iterate live** — POST progressively-richer profiles to
   `https://shumei-api.lastwargame.com:19091/deviceprofile/v4` until the reply is `1100` +
   `detail.deviceId`. `curl -X POST -H 'Content-Type: application/octet-stream' -d '<json>'`.
5. **Port the winning shape into `internal/shumei`** — replace `DeviceProfile`/`protect` with the
   real body + hybrid crypto; re-test `BoxID`.

## Current Go code (for the next agent)

- `internal/shumei/shumei.go` — `BoxID(client, deviceID, device)`, `Device` (clock, Wi-Fi and
  per-install fields, see `docs/shumei.mdx`), `DeviceProfile`, `ProfileResponse`,
  `protect()` (raw deflate → AES-256-CBC under `hex(MD5(r))` → base64 as `data`, plus
  RSA-OAEP of `r` as `ep` and of `"0"` as `tn`), embedded SPKI, constants. **Safe to
  ship**: `BoxID` returns `ErrUnassigned` on `1902`, and `main.go`'s `-cs-shumei-gen` logs a
  warning and falls back to empty `shumeiBoxId` (which the game server accepts). The flag is
  ignored in iOS mode (`-cs-ios` or a session config's `iosMode`), since the fingerprint is an
  Android profile.
- `internal/shumei/install.go` — `Install`, the values one install keeps (`a24`, `a84`, `a125`,
  `a126`, `a163`, `a167`/`a168`) and the deviceId they belong to, with `NewInstall`, `Fill`,
  `For` (binds an install to one deviceId), `Registered` and `Validate`.
- `internal/app/main.go` — `-cs-shumei-gen` flag wired into `runCrossServerTest`;
  `mintShumeiBoxID` fills the session config's `shumeiInstall` and writes it back.
- Docs: `docs/shumei.mdx` (protocol + status), `docs/auth.mdx`/`native-binaries.mdx`/`go-client-
  roadmap.mdx` reference it.
- `go build ./...`, `golangci-lint run ./internal/shumei/` (0 issues), `go test ./internal/shumei/`
  all green.

## Gotchas the next agent must not regress

- The SDK version is `"3.14.3"`, **not** `"3.14.3_build1"`. It is `a7` inside the device data
  (and cloudconf's `sdkver`), never a top-level deviceprofile field.
- RSA-OAEP label is empty; MGF1 = SHA-256; don't add a label or use PKCS#1 v1.5.
- The deviceprofile `data` field is a bare base64 string: AES-256-CBC (key `hex(MD5(r))`, IV
  `"0102030405060708"`) of the **raw**-deflated (`compress/flate`, not zlib) `aNN` JSON. The
  nested `data` object belongs to cloudconf.
- `retry`:`1` is **not** part of the first request; only the background resend adds it.
- The Go `DeviceProfile` struct is exactly the eight fields native `w1` writes (`organization,
  os, appId, encode, compress, data, tn, ep`); don't add cloudconf fields to it.
- The fingerprint is an Android profile, so never send it with an iOS Login (`-cs-ios` or a
  session config's `iosMode`).
- The Login's `shumeiBoxId` is `"B"` + `detail.deviceId`, not the bare `deviceId`.
- Organization `"9dNCYkjHeFMYZMOjxgX8"` is validated server-side (wrong value → `9101`).