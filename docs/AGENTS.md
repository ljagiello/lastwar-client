> **First-time setup**: Customize this file for your project. Prompt the user to customize this file for their project.
> For Mintlify product knowledge (components, configuration, writing standards),
> install the Mintlify skill: `npx skills add https://mintlify.com/docs`

# Documentation project instructions

## About this project

- This is a documentation site built on [Mintlify](https://mintlify.com)
- Pages are MDX files with YAML frontmatter
- Configuration lives in `docs.json`
- Use the Mintlify MCP server, `https://mcp.mintlify.com`, to edit content and settings via MCP
- Use the Mintlify docs MCP server, `https://www.mintlify.com/docs/mcp`, to query information about using Mintlify via MCP

## Terminology

- **SFS2X** — always this casing (not "SFS2x" or "sfs2x"). Short for SmartFoxServer 2X, the third-party wire protocol the client's transport layer reimplements by hand (data model reused, socket layer is not).
- **GSL** — Gate Server List, the RSA+AES-wrapped HTTP handshake used for server discovery ("the GSL flow" / "GSL crypto"). Spell out on first use per page, then use the acronym.
- **`goclient`** — the researcher's from-scratch Go reimplementation of the protocol; its source lives at the repository root (`identity.go`, `main.go`, ...), one level up from `docs/`. Use the lowercase, no-space form (`goclient` hardcoded...) when naming the actual codebase/package in prose, and "Go client" (capitalized, two words) in ordinary prose ("a Go client needs to...").
- **"confirmed live" / "live-tested"** — reserved for claims verified by actually round-tripping traffic against production servers. Keep this distinct from **"static-analysis-only"** claims (recovered from decompiled Lua/C# but never exercised against a real server). Never upgrade a static-analysis claim into unqualified "confirmed" language — the server can change, so point-in-time hedges are load-bearing, not decoration.
- **"the real account" / "a real account"** — the researcher's own production game account used for live-validation testing, as distinct from a freshly auto-created "guest account." Don't call it a "test account" — that phrasing has been normalized away in favor of "real account" for consistency.

## Style preferences

- Use active voice and second person ("you")
- Keep sentences concise — one idea per sentence
- Use sentence case for headings
- Bold for UI elements: Click **Settings**
- Code formatting for file names, commands, paths, and code references

## Content boundaries

This project documents the Last War: Survival Game mobile client's network protocol, crypto, and file formats — reverse-engineered from the APK for interoperability research, i.e. understanding the client-server contract well enough to build a compatible, from-scratch client. In scope: wire-protocol/codec internals, bootstrap and authentication flows, the command catalog, game-data table and Lua-bundle formats, and live-validation findings from packet capture against production.
It does not document gameplay strategy, economy optimization, or how to gain an unfair advantage — resource duplication, automation for competitive gain, or bypassing anti-cheat/anti-fraud systems are out of scope. This is protocol documentation for interoperability, not a hacking or cheating guide; content should stop at "how the protocol works," not extend into "how to exploit it against other players."
