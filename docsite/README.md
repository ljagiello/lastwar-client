# Last War protocol dossier

Documentation site for an unofficial, from-the-APK reverse-engineering effort covering *Last War:
Survival Game*'s (`com.fun.lastwar.gp`) network protocol, crypto, and file formats — assembled for
interoperability research, i.e. understanding the client-server contract well enough to build a
compatible, from-scratch client (`goclient/`, in the parent directory).

21 pages, organized to mirror the underlying research: bootstrap/crypto/wire-protocol fundamentals,
the full command and entity-ID catalogs, per-domain gameplay coverage (city, military, social,
economy, world), client-internals (Lua, data tables, Android/native layers), and — the most
load-bearing section — [live validation against production](./live-validation.mdx), which documents
what was confirmed by actually round-tripping traffic against real servers rather than static
analysis alone.

See `AGENTS.md` for project-specific terminology and content boundaries before editing.

## Development

Install the [Mintlify CLI](https://www.npmjs.com/package/mint) to preview changes locally:

```
npm i -g mint
```

Run from this directory (where `docs.json` lives):

```
mint dev
```

Local preview at `http://localhost:3000`. Check for broken internal links before committing:

```
mint broken-links
```

## Keeping this evergreen

This documents a live, third-party production service that can change its behavior at any time
without notice. A few things matter for keeping it trustworthy over time:

- Pages that make "confirmed live" / "Resolved" claims about production behavior include the
  `<LiveVerificationStatus />` snippet (`snippets/live-verification-status.mdx`) near the top,
  which states the date and client version those claims were last verified against. If you
  re-verify something, update that one snippet file rather than hunting down every page.
- Keep "confirmed live" / "live-tested" claims distinct from "static-analysis-only" claims — see
  `AGENTS.md`'s Terminology section. Don't upgrade a static-analysis guess into unqualified
  "confirmed" language just because it sounds more authoritative.
- Avoid session-relative language ("tonight," "this pass," "just tried") — write as if a reader
  is opening the page months later with no idea when it was authored.
- When a fix in `goclient/` changes what's true (a new confirmed building type, a corrected
  command, a newly-resolved open question), update the relevant page(s) *and* check
  `open-questions.mdx` / `research-pass.mdx` for anything that now contradicts the change.

## Publishing

Install the Mintlify GitHub app from your dashboard to auto-deploy on push to the default branch,
if/when this site is connected to a real Mintlify project. Not yet configured.
