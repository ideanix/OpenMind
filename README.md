# OpenMind

Self-hosted shared memory for AI coding agents. Any MCP client (Claude Code,
Codex, Cursor, Gemini CLI, your own scripts) reads and writes one team
knowledge base: small, attributable notes instead of lost transcripts.

Status: working MVP. Requirements: [docs/REQUIREMENTS.md](docs/REQUIREMENTS.md).

## Why

Agent memory today is per-machine, per-account, per-vendor, and auto-deleted.
Two people on one project cannot see what each other's agents learned.
OpenMind keeps one fact per note, with author, tool, session and revision
history, and serves it over MCP to every agent.

## Install

```bash
go install github.com/ideanix/openmind/cmd/openmind@latest
```

or build from source:

```bash
make build      # → bin/openmind
```

## Quick start (one machine, no auth)

```bash
openmind serve                       # http://localhost:7777/mcp, db in ~/.openmind/
openmind setup claude-code --server http://localhost:7777
# prints:  claude mcp add --transport http --scope user openmind http://localhost:7777/mcp
```

Then in any Claude Code session:

> Call `openmind_context` for project `myrepo` before you start.

## Team setup (self-hosted)

On a VPS:

```bash
OPENMIND_TOKENS="alice:$(openssl rand -hex 16),bob:$(openssl rand -hex 16)" \
openmind serve --addr :7777 --db /var/lib/openmind/openmind.db
```

Each person adds the server with their own token:

```bash
claude mcp add --transport http --scope user openmind https://mind.example.com/mcp \
  --header "Authorization: Bearer <token>"
```

Put a TLS-terminating proxy (Caddy, nginx) in front; OpenMind itself speaks
plain HTTP.

## MCP tools

| Tool | Purpose |
|---|---|
| `openmind_context(project)` | Digest of everything the team knows; call at task start. |
| `openmind_search(query, project, …)` | Ranked full-text search with snippets. |
| `openmind_get(id)` | Full note with provenance. |
| `openmind_list(project, since?, …)` | Recent changes. |
| `openmind_put(project, title, body, …)` | Create or update a note. Secrets are rejected. |
| `openmind_deprecate(id, reason)` | Retire a note; it stays in history. |

## CLI

```bash
openmind put --project logdoc --title "Tests need Redis" --tags testing \
  --body 'Run `make redis` before `go test ./...`. **Why:** the queue layer is not mocked.'

openmind search  --project logdoc redis
openmind context --project logdoc
openmind import claude-code --project logdoc          # from ~/.claude/projects/<cwd>/memory
openmind export claude-md   --project logdoc --out CLAUDE.md   # idempotent, between markers
```

Point the CLI at a server with `--server URL --token T` or
`OPENMIND_SERVER` / `OPENMIND_TOKEN`; without them it uses the local database.

## REST API

All endpoints take `Authorization: Bearer <token>` when tokens are configured.

```
GET    /api/v1/notes?project=&q=&type=&tags=&since=&status=&limit=
POST   /api/v1/notes                 {project,title,body,type,scope,tags,id?}
GET    /api/v1/notes/{id}
GET    /api/v1/notes/{id}/history
DELETE /api/v1/notes/{id}?reason=    (deprecate)
GET    /api/v1/projects
GET    /api/v1/projects/{project}/context
GET    /api/v1/whoami
POST   /mcp                          MCP Streamable HTTP
```

## Note format

Markdown with YAML frontmatter, a superset of Claude Code's auto-memory
files, so existing memory folders import unchanged:

```markdown
---
id: 01a0aef1b6336fa7086a
title: API tests need local Redis
type: project          # user | feedback | project | reference | decision
scope: project         # personal | project | team
project: logdoc
tags: [testing, redis]
author: alice
source: {tool: claude-code, session: 5de3c033-…}
status: active
---
Run `make redis` before `go test ./...`.
**Why:** the queue layer is not mocked.
```

## Roadmap

See [docs/REQUIREMENTS.md](docs/REQUIREMENTS.md) §10. Next: `openmind init`,
two-way sync with Claude Code memory, hooks plugin, roles, web UI, Docker image.

## License

Apache-2.0.
