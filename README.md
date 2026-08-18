# lastwar-client

Last War: Survival Game — unofficial Go client and protocol dossier, reverse-engineered from
the decompiled APK for interoperability research.

This repo is the one-stop home for both halves of that work:

- **[`goclient/`](goclient/)** — a from-scratch Go reimplementation of the client's network layer:
  GSL RSA+AES bootstrap crypto, the SFS2X wire protocol, the SFSObject binary codec, and
  resource-collection automation, all live-tested against production. Start with
  [`goclient/README.md`](goclient/README.md) for build/run instructions and current status.
- **[`docsite/`](docsite/)** — the full protocol dossier as a [Mintlify](https://mintlify.com)
  site: wire format, crypto, the ~3,178-command catalog, entity IDs, and a running log of what's
  been confirmed live against real production servers versus what's still static analysis. Preview
  it locally with `mint dev` from inside `docsite/`.

## Status

Both are actively maintained together — protocol findings from live testing feed straight back
into the dossier, and dossier gaps drive what gets tested next. See
[`docsite/live-validation.mdx`](docsite/live-validation.mdx) for the current confirmed-vs-unconfirmed
picture, and [`goclient/README.md`](goclient/README.md)'s Status section for the client specifically.

## Scope

Interoperability research: understanding the client-server contract well enough to build a
compatible, from-scratch client. Not in scope: gameplay strategy, economy optimization, or
anything that reads as a cheating/exploit guide rather than protocol documentation — see
[`docsite/AGENTS.md`](docsite/AGENTS.md) for the full boundary.
