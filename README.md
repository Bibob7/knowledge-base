# knowledge-base

A local knowledge base for Claude: an MCP server in Go (stdio) on Postgres + pgvector.
Hybrid search combining vector and full-text search (Reciprocal Rank Fusion).

## Installation (macOS/Linux)

Prerequisites: Go (version according to `go.mod`), Docker with `docker compose`, [Ollama](https://ollama.com/download)
(or an OpenAI key) and optionally Claude Code.

```bash
git clone <repo> && cd knowledge-base
./install.sh
```

The installer is idempotent (running it again updates) and does the following:

1. Creates `.env` with a random Postgres password (mode 600)
2. Starts Postgres + pgvector via Docker
3. Pulls the embedding model `nomic-embed-text` via Ollama
4. Builds the MCP server into `bin/` and tests it with `bin/knowledge-base-mcp --check`
5. After asking, copies the rules file to `~/.claude/rules/`, registers the MCP server with Claude Code and adds the hooks to `~/.claude/settings.json`

Then restart Claude Code. The server finds its configuration in `.env` on its own
(variables from the environment take precedence); `~/.claude.json` contains no credentials.

| Option | Effect |
|---|---|
| `--yes` | no questions asked |
| `--no-claude` | skip the Claude integration |
| `--no-ollama` | do not pull a model |
| `--embedding openai` | OpenAI instead of Ollama (`OPENAI_API_KEY` in the environment) |
| `--database-url URL` | use an existing Postgres with pgvector instead of Docker |
| `--uninstall` | remove registration, hooks and rules file, stop containers (data is kept) |
| `--uninstall --purge` | additionally delete the data volume and `.env` |

### Shared server

Several people can share a central database: run Postgres with pgvector on a server and run
`./install.sh --database-url postgres://...` for each user. Everyone must use the same embedding model.
The MCP server itself still runs locally per user (stdio). The database should then only be reachable
over TLS and with its own password.

### Manual installation

```bash
cp .env.example .env && docker compose up -d
ollama pull nomic-embed-text
go build -o bin/knowledge-base-mcp ./cmd/knowledge-base-mcp
claude mcp add knowledge-base --scope user -- "$PWD/bin/knowledge-base-mcp"
cp claude/rules/knowledge-base.md ~/.claude/rules/
bin/knowledge-base-mcp install-hooks
```

Different embedding model (e.g. OpenAI): adjust `EMBEDDING_*` in `.env`, see `.env.example`.
Claude Desktop: the same command as an entry in `claude_desktop_config.json`.

## Tools

| Tool | Purpose |
|---|---|
| `remember` | Remember a short fact. Returns similar existing entries so that contradictions are noticed. With `supersedes` it replaces old entries. |
| `supersede` | Mark entries as outdated (correction). They are no longer found but are kept as history. |
| `search_knowledge` | Hybrid search (active entries only); with `project` = that project + cross-project knowledge |
| `list_documents` | List entries, e.g. "what was learned recently" (`include_superseded` also shows outdated ones) |
| `get_document` | Read an entry by ID |
| `add_document` | Store a longer document. The same `source` replaces the old one. |
| `delete_document` | Delete permanently (use `supersede` for corrections) |

Entries have an optional `project` (empty = cross-project), a `kind` (`fact`/`document`)
and a `status` (`active`/`superseded`). Texts containing obvious secrets (keys, tokens, passwords)
are rejected.

## Automatic retrieval (hooks)

A Claude Code hook (`UserPromptSubmit`, `bin/knowledge-base-mcp hook`) automatically feeds knowledge matching a
concrete task into the context: the up to 4 best-matching entries for the prompt (project + cross-project).
Short prompts (< 20 characters) and slash commands are skipped; whatever was already injected in the session
is not injected again. There is deliberately no hook at session start: knowledge should be retrieved per task,
not wholesale. The hook is only a safety net; the lookup itself is driven by the rules file (Claude searches on
its own when a task calls for it).

A hook never disturbs the session: on errors (Postgres down, Ollama unreachable, timeout of 8 s) it stays
silent and exits with code 0. A call typically takes under 100 ms.

```bash
bin/knowledge-base-mcp install-hooks     # add to ~/.claude/settings.json (backup: settings.json.kb-backup)
bin/knowledge-base-mcp uninstall-hooks   # remove only our own entries
```

The installer does this automatically. Existing hooks and the key order are preserved;
restart Claude Code after changes.

Relevance filter: only hits with a cosine similarity >= 0.67 are injected (`KB_HOOK_MIN_SIM`).
The value is calibrated for `nomic-embed-text` (unrelated prompts reach about 0.55-0.66, matching ones
0.67-0.77) and deliberately prefers missing a hit over showing noise on every prompt.
Troubleshooting: `KB_HOOK_DEBUG=1` prints all candidates with their similarity to stderr, e.g.

```bash
echo '{"session_id":"t","cwd":"/path/to/repo","hook_event_name":"UserPromptSubmit","prompt":"..."}' \
  | KB_HOOK_DEBUG=1 bin/knowledge-base-mcp hook
```

## Learning at session end (safety net)

In addition to the immediate `remember` during the session, a `SessionEnd` hook evaluates the transcript:

1. The hook starts a detached background process (`bin/knowledge-base-mcp extract`) and returns immediately,
   so ending the session is not delayed.
2. The process reads only your messages and Claude's text (no tool output, no subagents, no system
   messages). Sessions with fewer than 3 user messages are skipped, very long ones are cut to 100,000
   characters, and resumed sessions are evaluated only from the last evaluated line onwards.
3. A lean `claude -p` run (model `sonnet`, without tools, MCP, hooks and settings) extracts corrections,
   preferences/decisions and verified facts that cannot be read from the code. The prompt is deliberately
   strict: better 0-2 strong facts than many weak ones.
4. Every fact is checked against the knowledge base: similarity >= 0.92 counts as known and is discarded;
   between 0.78 and 0.92 a second model call decides `add`, `skip` or `supersede` (only with IDs that were
   presented to it; when in doubt `skip`). Secrets are filtered out.
5. Stored entries carry the tag `auto`; corrections additionally `correction`. To review:
   `list_documents` with `tag: auto`, or `tail ~/Library/Caches/knowledge-base/extract.log` (Linux: `~/.cache/knowledge-base/`).

The evaluation uses your Claude subscription or API access (typically a few cents per long session); the
transcript goes to the same provider as your regular session. A guard prevents the background run from
triggering evaluations or hooks itself.

| Setting (`.env` or environment) | Effect |
|---|---|
| `KB_AUTO_LEARN=0` | disable the evaluation at session end (the hook stays registered but does nothing) |
| `KB_EXTRACT_MODEL` | model used for the evaluation (default `sonnet`) |
| `KB_CLAUDE_BIN` | path to the `claude` CLI if it is not on the `PATH` |

Test manually without storing anything:

```bash
bin/knowledge-base-mcp extract --transcript ~/.claude/projects/<project>/<session>.jsonl \
  --session test --project my-project --dry-run
```

Not covered: sessions that are terminated abruptly (terminal closed, crash) may not trigger
`SessionEnd`. On Windows the background evaluation is not implemented.

## Automatic learning

The behavior is controlled by the rules file `claude/rules/knowledge-base.md` (copied by the installer to
`~/.claude/rules/` and loaded automatically by Claude Code): search first when working on a task, call
`remember` immediately on corrections/decisions/findings, replace outdated entries via `supersede`.

## Notes

- **Do not change the embedding model afterwards.** The vector dimension is fixed in the schema on first start; the server aborts if `EMBEDDING_DIM` differs. When switching models, delete the volume (`docker compose down -v`) and re-ingest.
- The server works with any OpenAI-compatible `/embeddings` endpoint (Ollama, OpenAI, LM Studio, vLLM).
- Data lives in the Docker volume `kb-pgdata`. Backup: `docker exec knowledge-base-postgres pg_dump -U kb knowledge_base > backup.sql`
- Postgres is bound to `127.0.0.1` only. The default credentials (`kb`/`kb`) are meant for local use only.
- Tests: `go test ./...`
