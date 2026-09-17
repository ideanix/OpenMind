# OpenMind — Requirements

Status: draft v0.2 · 2026-09-17 · Owner: Evgenii (inerc)

OpenMind is an open-source, self-hosted knowledge layer that lets a team share
what their AI coding agents have learned. Any agent (Claude Code, Codex, Cursor,
Gemini CLI, a custom script) reads and writes the same knowledge base over MCP,
regardless of vendor, model, or machine.

---

## 1. Problem

Every AI coding tool keeps its memory in a private, per-machine, per-account
store. When two people work on the same project:

- Context accumulated in one developer's sessions is invisible to the other.
- Switching tools (Claude Code → Codex) or models loses everything.
- Transcripts get auto-deleted (Claude Code: 30 days by default), so knowledge
  silently disappears.
- The only portable channel today is a hand-written `CLAUDE.md` / `AGENTS.md`,
  which nobody keeps up to date.

Existing tools cover pieces of this (see §11), but none combine: vendor-neutral
MCP access, self-hosting, team scopes, provenance, and human review of what
becomes shared knowledge.

## 2. Vision

One shared brain per team. An agent starting a session on any machine, in any
tool, sees what the team already knows about the project: decisions, gotchas,
conventions, environment facts, current status. Knowledge is small, reviewed,
attributable, and owned by the team, not by a vendor.

## 2a. Deployment model

OpenMind is built as a service from day one, delivered in three steps:

1. **Local** — one binary on a laptop, `openmind serve` or `openmind mcp`
   over stdio, no auth. Used to share context between one person's projects
   and tools.
2. **Self-hosted / on-premise** — the same binary on a VPS or inside the
   company network, bearer tokens per user, one SQLite file per instance.
   This is what a small team runs.
3. **SaaS** — the same code behind a control plane that provisions an
   instance (or a tenant) per team, adds sign-up, billing, SSO and backups.

To keep step 3 cheap, the core must stay tenant-ready: every note carries a
`project`, every request carries an identity, all state lives in one data
directory, the HTTP surface is the only surface (MCP over HTTP, REST), and no
feature may assume a single user or a local filesystem.

## 3. Goals

- G1. Vendor-neutral: works with any MCP client; no lock-in to one agent or LLM.
- G2. Self-hosted: single binary, one data directory, runs on a laptop or a VPS.
- G3. Team-scoped: personal / project / team visibility with simple access control.
- G4. Reviewable: knowledge is markdown, diffable, with history and provenance.
- G5. Zero-LLM core: search and sync work without any model or API key.
  LLM-powered features (distillation, dedup) are optional plugins.
- G6. Easy adoption: `openmind serve` + one line in the agent's MCP config.

## 4. Non-goals (v1)

- Not a vector database or RAG framework for arbitrary documents.
- Not a chat history archive; transcripts are a *source*, not what we store.
- Not a replacement for `CLAUDE.md`; OpenMind can *generate* it, not replace it.
- No multi-tenant SaaS, billing, or SSO in v1.
- No real-time collaborative editing UI in v1.

## 5. Users and scenarios

**Personas**

- *Solo dev with several tools* — wants Claude Code and Codex to share memory.
- *Two-person team* (the LogDoc case) — one partner has months of context, the
  other starts from zero.
- *Small company team* — 5–20 devs, several repos, wants a reviewed knowledge
  base with an audit trail and no secrets leaking.

**Key scenarios**

- S1. Alice's agent learns "the API tests need local Redis"; Bob's agent knows
  it in his next session without anyone telling him.
- S2. A new teammate runs `openmind init` in the repo and their agent immediately
  has project context.
- S3. Alice switches from Claude Code to Codex; memory follows her.
- S4. A note is wrong. Bob edits it; the change is versioned and attributed.
- S5. An agent tries to save an API key; OpenMind refuses and logs the attempt.
- S6. Alice imports six months of Claude Code transcripts; OpenMind extracts
  candidate notes for her review (optional LLM plugin).

## 6. Core concepts

**Note** — the unit of knowledge. One fact, decision, or instruction per note.
Stored as markdown with YAML frontmatter, compatible with Claude Code's
auto-memory format so existing memory folders import without conversion.

```markdown
---
id: 01J9…                # ULID
title: API tests need local Redis
type: project            # user | feedback | project | reference | decision
scope: project           # personal | project | team
project: logdoc
tags: [testing, redis]
author: alice
source:                  # provenance
  tool: claude-code
  session: 5de3c033-…
  model: claude-opus-5
created: 2026-09-17T10:00:00Z
updated: 2026-09-17T10:00:00Z
expires: null            # optional TTL for time-bound facts
status: active           # draft | active | deprecated
---
Integration tests under `logdoc/tests/` fail without Redis on :6379.
**Why:** the queue layer is not mocked.
**How to apply:** run `make redis` before `go test ./...`.
```

**Scope** — who can see a note: `personal` (author only), `project` (everyone
with access to the project), `team` (all members).

**Project** — a named knowledge space, usually mapped to one or more git remotes.

**Provenance** — which person, tool, session, and model produced a note.
Never optional.

**Revision** — every write creates a new revision; old ones are kept.

## 7. Functional requirements

### 7.1 Server

- FR-1. Single Go binary `openmind` with subcommands `serve`, `init`, `sync`,
  `import`, `export`, `note`, `search`, `user`, `token`.
- FR-2. `serve` exposes MCP over HTTP (Streamable HTTP) and stdio, plus a REST
  API. The MCP tool surface must be identical over both transports.
- FR-3. Storage in SQLite (WAL mode) with FTS5 full-text search. One file,
  copyable backup.
- FR-4. Optional embedding provider interface (Ollama, OpenAI-compatible,
  Anthropic) for hybrid search. Off by default; lexical search must be good
  enough alone.
- FR-5. Every note has full revision history; `GET /notes/{id}/history`.
- FR-6. Soft delete (`status: deprecated`) by default; hard delete only by an
  admin with a flag.

### 7.2 MCP tools (the contract every agent sees)

| Tool | Purpose |
|---|---|
| `openmind_search(query, project?, scope?, tags?, limit?)` | Ranked notes with snippets. |
| `openmind_get(id)` | Full note. |
| `openmind_list(project?, type?, since?)` | Browse / recent changes. |
| `openmind_put(note)` | Create or update; returns id and revision. |
| `openmind_deprecate(id, reason)` | Mark obsolete. |
| `openmind_context(project)` | Compact digest for session start (≤ 25 KB). |

- FR-7. `openmind_context` returns a ready-to-inject digest: the index of
  active notes for the project, ordered by recency and pin status, capped in
  size so it fits the client's startup budget.
- FR-8. Tool descriptions must instruct the agent when to save (corrections,
  decisions, environment facts) and when not to (anything derivable from code,
  secrets, transient state). This mirrors Claude Code's own memory guidance.
- FR-9. MCP resources: `openmind://project/{name}/index` for clients that
  prefer resources over tools.

### 7.3 CLI and client integration

- FR-10. `openmind init` in a repo: detects git remote, creates or links the
  project, writes MCP config snippets for detected clients (Claude Code,
  Codex, Cursor, Gemini CLI) after confirmation.
- FR-11. `openmind sync` two-way syncs a local folder of markdown notes
  (default: Claude Code's `~/.claude/projects/<project>/memory/`) with the
  server. Conflicts resolved by revision; the losing side is kept as a
  revision, never discarded.
- FR-12. `openmind import --from claude-code <path>` ingests existing memory
  folders as notes with provenance. Transcript import (`.jsonl`) produces
  *candidate* notes in `draft` status; nothing becomes `active` without a
  human or an explicitly enabled distiller plugin.
- FR-13. `openmind export --format claude-md` renders a project's active notes
  into a `CLAUDE.md` / `AGENTS.md` section between marker comments, so teams
  that only commit files still benefit.
- FR-14. Claude Code hooks shipped as a plugin: `SessionStart` pulls
  `openmind_context`; `Stop` reminds the agent to save new learnings.

### 7.4 Access control

- FR-15. Users and teams; bearer tokens per user, scoped per project.
- FR-16. Roles per project: `reader`, `writer`, `maintainer`. Only
  maintainers can hard-delete, change scopes from personal to team, or edit
  others' notes.
- FR-17. Personal-scope notes are never returned to other users, including
  through search snippets.

### 7.5 Secret hygiene

- FR-18. Every write passes a secret scanner (regex set covering cloud keys,
  tokens, private keys, connection strings). A hit rejects the write with the
  pattern name; the note is not stored.
- FR-19. Transcript import redacts secrets at ingestion time before anything
  touches disk.
- FR-20. Scanner rules are extensible via a config file.

### 7.6 Web UI (minimal, v1)

- FR-21. Read-only browse and search of notes, revision diff view, and a
  "pending drafts" queue for review. Server-rendered, no JS build step.
- FR-22. Editing in the UI is a v1.1 feature.

### 7.7 Optional plugins (v1.1+)

- FR-23. Distiller: LLM extracts candidate notes from transcripts. Provider
  pluggable; runs only when enabled.
- FR-24. Deduplicator: flags near-duplicate notes for merge.
- FR-25. Git backend: notes mirrored to a git repo for PR-based review.

## 8. Non-functional requirements

- NFR-1. Search latency < 50 ms p95 for 100k notes on a laptop.
- NFR-2. `openmind_context` response < 25 KB and < 100 ms.
- NFR-3. Binary size < 30 MB; no CGO if a pure-Go SQLite driver suffices.
- NFR-4. Runs on macOS, Linux, Windows; arm64 and amd64.
- NFR-5. No outbound network calls unless a plugin is explicitly enabled.
- NFR-6. Data directory is fully portable: copy it, run `serve`, done.
- NFR-7. All APIs versioned; MCP tool names stable across minor versions.
- NFR-8. Test coverage: unit tests for storage, scanner, sync; integration
  tests driving the MCP surface with a real client library.

## 9. Architecture constraints

- Language: Go 1.24+, single module.
- MCP: official Go SDK (`modelcontextprotocol/go-sdk`).
- Storage: SQLite + FTS5; schema migrations embedded.
- HTTP: standard library `net/http`; no heavy frameworks.
- Config: one YAML file plus env overrides; sane defaults with no config.
- License: Apache-2.0.
- Repo language: English only (code, docs, commits).

## 10. Delivery plan

**Phase 0 — spec and skeleton (this document).**

**Phase 1 — quick local/self-hosted version (built 2026-09-17).**
Server with SQLite/FTS5, MCP over stdio and Streamable HTTP, the six tools,
bearer tokens mapped to user names, project/personal scopes, secret scanner,
revision history, REST API, CLI (`put/search/list/get/context/projects`),
`import claude-code`, `export claude-md`, `setup claude-code`.
Not in this cut: `init`, two-way `sync`, hooks plugin, web UI, roles.

Acceptance (met in a smoke test): two users with different tokens against one
server; a note written by one is found by the other via MCP search and
appears in `openmind_context`; secrets are rejected; export is idempotent.

**Phase 1.1 — team hardening.**
`openmind init` (git-remote → project mapping, MCP config writing), two-way
`sync` with Claude Code's memory folder, Claude Code hooks plugin
(SessionStart → context), roles (reader/writer/maintainer), read-only web UI,
Docker image and systemd unit, backups.

**Phase 2 — quality.**
Hybrid search with optional embeddings, revision diff UI, UI editing,
dedup suggestions, Codex/Cursor/Gemini config generators verified end-to-end.

**Phase 3 — team scale.**
Git backend, distiller plugin, audit log export, OIDC login.

## 11. Landscape and positioning

| Project | What it does | Gap OpenMind fills |
|---|---|---|
| deja-vu | Indexes local transcripts of 25 agents, MCP recall, Go binary | Per-machine; raw transcripts, no curated notes, no team roles |
| claude-mem-sync | Syncs claude-mem observations via git + CI bot | Claude Code only; no MCP for other agents |
| Basic Memory | Markdown knowledge graph, MCP, AGPL | Personal-first; team is a paid cloud |
| OpenMemory (Mem0) | Local MCP memory server | Single user; LLM required for extraction |
| Deeplake Hivemind | Shared team memory for MCP clients | Commercial, not self-hosted |
| Claude Code team memory | Vendor sync of `memory/team/` | Claude-only, Anthropic-hosted, undocumented |

Positioning: the reviewed, attributable, self-hosted team layer that any agent
can use. Small notes, not transcripts. Humans stay in the loop.

## 12. Open questions

- OQ-1. Name: `OpenMind` collides with an existing GitHub project
  (iggyghub/OpenMind, a personal agent platform) and openmind.org. Decide
  before publishing. Candidates: keep as working title, or rename.
- OQ-2. Should `personal` scope live on the server at all, or stay local-only
  and only `project`/`team` sync?
- OQ-3. How aggressively should `openmind_context` summarize versus list?
  Listing is deterministic; summarizing needs an LLM.
- OQ-4. Conflict policy for `sync` when both sides edited the same note:
  last-writer-wins with history, or force a manual merge?
- OQ-5. Do we ship a hosted demo, or self-host only?
  **Resolved 2026-09-17:** self-hosted first, SaaS later on the same
  binary; see §2a.

## 13. Glossary

- **MCP** — Model Context Protocol, the standard agents use to call tools.
- **Note** — one unit of knowledge, markdown with frontmatter.
- **Scope** — visibility level of a note.
- **Provenance** — who/what/when produced a note.
- **Digest** — the compact project index returned by `openmind_context`.
