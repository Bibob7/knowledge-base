# knowledge-base

[![CI](https://github.com/Bibob7/knowledge-base/actions/workflows/ci.yml/badge.svg)](https://github.com/Bibob7/knowledge-base/actions/workflows/ci.yml)

A persistent memory for Claude Code that learns across sessions and projects: an MCP server in Go (stdio) with
hybrid search (vector + full text, Reciprocal Rank Fusion) on a local SQLite file, or optionally on Postgres with
pgvector (e.g. a server shared by a team). It retrieves matching knowledge
for each prompt, stores corrections, decisions and verified facts as they come up, replaces outdated entries instead
of piling them up, and evaluates sessions afterwards as a safety net. Everything runs locally.

## Installation (Claude Code plugin, macOS/Linux)

Prerequisite: [Ollama](https://ollama.com/download) for local embeddings (without it, OpenAI embeddings:
`--embedding openai`). The database is a SQLite file; SQLite is built into the server binary, so no database
server or Docker is needed. The server
binary is downloaded from the [GitHub release](https://github.com/Bibob7/knowledge-base/releases) of the plugin
version (macOS and Linux, amd64 and arm64); [Go](https://go.dev/dl/) 1.27 or newer is only needed where no release binary exists.

In Claude Code:

```
/plugin marketplace add Bibob7/knowledge-base
/plugin install knowledge-base@bibob7
/knowledge-base:setup
```

`/knowledge-base:setup` lets Claude check the prerequisites, explain
what is missing and run `scripts/setup.sh`, which is idempotent (running it again repairs and updates):

1. Creates `~/.config/knowledge-base/config.env` (mode 600)
2. Pulls the embedding model `bge-m3` via Ollama
3. Installs the server binary into the plugin data directory
4. Creates the database `~/.local/share/knowledge-base/knowledge.db` (mode 600) and runs the self-test (`--check`)

Then run `/reload-plugins` or restart Claude Code. The plugin brings everything else along:

| Part | What it does |
|---|---|
| MCP server `kb` (`.mcp.json`) | the tools below; its server instructions tell Claude when to search, remember and supersede (`cmd/knowledge-base-mcp/instructions.md`) |
| Hooks (`hooks/hooks.json`) | retrieval per prompt, learning at session end, silent sweep of crashed sessions |
| Skill `/knowledge-base:setup` | setup, repair and status |
| Skill `/knowledge-base:viz` | opens the live visualisation in the default browser (see [Visualisation](#visualisation)) |

`scripts/kb` is the launcher used by the MCP server and the hooks: it installs `knowledge-base-mcp` once per plugin
version into `${CLAUDE_PLUGIN_DATA}/bin/` (`~/.claude/plugins/data/knowledge-base-bibob7/bin/`) and runs it. It
downloads the release binary and checks it against the release's `checksums.txt`; if there is none for the version or
platform, it builds from source with Go. A hook never installs: until the binary exists, hooks do nothing. The configuration and the database live outside the plugin
and survive updates and uninstalling it.

| `scripts/setup.sh` option | Effect |
|---|---|
| `--status` | only report what is installed and what is missing |
| `--database-url URL` | use another database: `postgres://...` (Postgres with pgvector) or `sqlite:///path.db` |
| `--postgres` | run a local Postgres with pgvector in Docker instead of SQLite |
| `--migrate-to-sqlite` | move a Postgres knowledge base into the SQLite file (see below) |
| `--embedding openai` | OpenAI instead of Ollama (`OPENAI_API_KEY` in the environment) |
| `--no-ollama` | do not pull a model |
| `--import-env PATH` | take over an existing configuration file |
| `--remove-legacy` | remove a pre-plugin installation (see below) |
| `--purge` | **delete** all knowledge: the SQLite file, the Docker Postgres with its volume, and the configuration |

Uninstall: `/plugin uninstall knowledge-base@bibob7` (the database is kept; `scripts/setup.sh --purge` deletes it).

### Upgrading from the pre-plugin installation

Older versions were installed with `install.sh`: hooks in `~/.claude/settings.json`, the rules file
`~/.claude/rules/knowledge-base.md` and an MCP server registered with `claude mcp add`. Running both would duplicate
every hook and tool. `/knowledge-base:setup` detects it; `setup.sh --remove-legacy` removes it and takes over its
`.env` if there is one. The data volume is the same, so all entries are kept.

### Moving from Postgres in Docker to SQLite

Up to version 0.5 the knowledge base ran in Postgres in Docker. Such an installation keeps working after the update.
To drop Docker, run `/knowledge-base:setup --migrate-to-sqlite`:
- It copies every entry with its ID, history, tags, timestamps and embeddings into the SQLite file and checks the counts.
- It switches `DATABASE_URL`; the old URL stays in the configuration as a comment.
- It stops the container.

Run `/reload-plugins` right after it (until then running sessions still write into Postgres). The Docker
volume stays as a backup; once everything works, delete it with `docker volume rm knowledge-base_kb-pgdata`. The
command `"$KB" migrate --from URL --to URL` copies between any two databases, in both directions.

### Which database?

SQLite is the default and fits a personal knowledge base. Search compares the query with every stored vector exactly
(no approximate index), plus FTS5 full text. Opening the file and searching 10,000 chunks with 1024 dimensions, as
the hook does per prompt, takes about 80 ms on an M1 Pro. Parallel sessions, hooks and the evaluation share the
file (WAL mode). Postgres with pgvector (HNSW index) is the choice for a server shared by several people or for
far more than ~50,000 chunks.

### Shared server

Several people can share a central database: run Postgres with pgvector on a server and run
`/knowledge-base:setup --database-url postgres://...` for each user (an existing personal knowledge base can be
copied there with `"$KB" migrate --to postgres://...`). Everyone must use the same embedding model.
The MCP server itself still runs locally per user (stdio). The database should then only be reachable
over TLS and with its own password.

### Command line

The server binary also has helper commands (`viz`, `reindex`, `migrate`, `extract`, `sweep`). It finds the configuration in
`~/.config/knowledge-base/config.env` on its own (variables from the environment take precedence; `KB_ENV_FILE`
points to another file):

```bash
KB="$(ls ~/.claude/plugins/data/knowledge-base-bibob7/bin/knowledge-base-mcp-* | tail -n1)"
"$KB" --check
```

Development: `claude --plugin-dir .` loads the working copy; `KB_FORCE_BUILD=1 scripts/kb --ensure` builds the
binary for the current version from source (instead of the release binary).

### Releases

Bump `version` in `.claude-plugin/plugin.json`, merge, then tag the same version: `git tag v0.5.0 && git push origin
v0.5.0`. The release workflow checks that tag and version match, runs the tests and publishes the binaries for
macOS/Linux (amd64, arm64) with `checksums.txt`. Users who update the plugin before the release exists build from
source (if Go is installed), so tag right after merging the version bump.

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

A Claude Code hook (`UserPromptSubmit`, `"$KB" hook`) automatically feeds knowledge matching a
concrete task into the context: the up to 4 best-matching entries for the prompt (project + cross-project).
Short prompts (< 20 characters) and slash commands are skipped; whatever was already injected in the session
is not injected again. There is deliberately no hook at session start: knowledge should be retrieved per task,
not wholesale. The hook is only a safety net; the lookup itself is driven by the server instructions (Claude searches on
its own when a task calls for it).

A hook never disturbs the session: on errors (database unreachable, Ollama unreachable, timeout of 8 s) it stays
silent and exits with code 0. A call typically takes under 100 ms.

Relevance filter: only hits with a cosine similarity above the model's threshold are injected
(`KB_HOOK_MIN_SIM`, see [Embedding model](#embedding-model)). For `bge-m3` it is 0.52: off-topic prompts stayed
below 0.483 while matching entries were mostly above 0.5. On the live knowledge base 20 of 23 matching prompts
got the right entry and 0 of 9 off-topic prompts triggered an injection. The filter deliberately prefers
missing a hit over showing noise on every prompt.
Troubleshooting: `KB_HOOK_DEBUG=1` prints all candidates with their similarity to stderr, e.g.

```bash
echo '{"session_id":"t","cwd":"/path/to/repo","hook_event_name":"UserPromptSubmit","prompt":"..."}' \
  | KB_HOOK_DEBUG=1 "$KB" hook
```

## Learning at session end (safety net)

In addition to the immediate `remember` during the session, a `SessionEnd` hook evaluates the transcript:

1. The hook starts a detached background process (`"$KB" extract`) and returns immediately,
   so ending the session is not delayed.
2. The process reads only your messages and Claude's text (no tool output, no subagents, no system
   messages). Sessions with fewer than 3 user messages are skipped, very long ones are cut to 100,000
   characters, and resumed sessions are evaluated only from the last evaluated line onwards.
3. A lean `claude -p` run (model `sonnet`, without tools, MCP, hooks and settings) extracts corrections,
   preferences/decisions and verified facts that cannot be read from the code. The prompt is deliberately
   strict: better 0-2 strong facts than many weak ones.
4. Every fact is checked against the knowledge base: a near-verbatim copy (similarity >= 0.98) counts as known
   and is discarded; from the model's "similar" threshold upwards (`bge-m3`: 0.70) a second model call decides
   `add`, `skip` or `supersede` (only with IDs that were presented to it; when in doubt `skip`). Similarity
   alone cannot tell a contradiction from a paraphrase (both score 0.89-0.96), so it never discards a
   correction by itself. Secrets are filtered out.
5. Stored entries carry the tag `auto`; corrections additionally `correction`. To review:
   `list_documents` with `tag: auto`, or `tail ~/Library/Caches/knowledge-base/extract.log` (Linux: `~/.cache/knowledge-base/`).

The evaluation uses your Claude subscription or API access (typically a few cents per long session); the
transcript goes to the same provider as your regular session. A guard prevents the background run from
triggering evaluations or hooks itself.

| Setting (`config.env` or environment) | Effect |
|---|---|
| `KB_AUTO_LEARN=0` | disable the evaluation at session end (the hook stays registered but does nothing) |
| `KB_EXTRACT_MODEL` | model used for the evaluation (default `sonnet`) |
| `KB_CLAUDE_BIN` | path to the `claude` CLI if it is not on the `PATH` |
| `KB_SWEEP_IDLE` / `KB_SWEEP_MAX_AGE` | idle time before a transcript is swept (default `30m`) / maximum age (default `168h`) |
| `KB_CACHE_DIR` | directory for the log and the progress files (default: user cache dir, e.g. for tests) |

Test manually without storing anything:

```bash
"$KB" extract --transcript ~/.claude/projects/<project>/<session>.jsonl \
  --session test --project my-project --dry-run
```

On Windows the background evaluation is not implemented.

### Sessions that never end cleanly

When a terminal is closed or Claude Code crashes, `SessionEnd` never fires. A `SessionStart` hook therefore
starts a **silent** background sweep (`"$KB" sweep`) whenever a session starts or resumes
(not on `compact`). It prints nothing, so no knowledge is injected at session start; it only looks for
transcripts that were never evaluated:

- the transcript has not changed for 30 minutes (`KB_SWEEP_IDLE`), so sessions that are still open in another
  window are left alone, and the session that triggered the sweep is skipped;
- it is not older than 7 days (`KB_SWEEP_MAX_AGE`) and not older than the first sweep, so your old history is
  never processed retroactively (the very first sweep only records its start time);
- it was not evaluated up to its last change; resumed sessions are evaluated only from the last evaluated line;
- at most 3 transcripts per sweep (newest first), at most one sweep per 10 minutes, one sweep at a time, and
  one evaluation per session at a time; a session whose evaluation failed is retried after an hour.

The project is taken from the working directory recorded in the transcript. Show what a sweep would do without
storing anything: `"$KB" sweep --dry-run --force`.

## Automatic learning

The behavior is controlled by the MCP server instructions (`cmd/knowledge-base-mcp/instructions.md`, embedded in
the binary; Claude Code puts them into the system prompt and cuts them after 4096 characters, a test guards the
size): search first when working on a task, call `remember` immediately on corrections/decisions/findings, replace
outdated entries via `supersede`.

## Visualisation

In Claude Code: `/knowledge-base:viz` opens it in the default browser (`/knowledge-base:viz stop` stops it). From the
shell:

```bash
"$KB" viz --open             # foreground, Ctrl+C stops it; http://127.0.0.1:8765/
"$KB" viz --detach --open    # background (reuses a running instance), what the skill runs
"$KB" viz --stop             # stop the background instance
```

In the background the server stops on its own 30 minutes after the last request (`--idle`); an open tab polls every
10 seconds, background tabs at least once a minute, so it only stops after the tab was closed. The pid file and log
are in the cache directory (`viz.pid`, `viz.log`).

A live, read-only view of the database: key figures, growth over time, entries per project, a **knowledge map**
(entries placed by semantic similarity via a PCA of the embeddings; hollow points are outdated, dashed lines lead to
the entry that replaced them) and a filterable list with the full text. The page refreshes itself every 10 seconds.
It needs no internet access (no external scripts or fonts). There is no authentication, so the server only listens on
loopback addresses and rejects requests with a foreign `Host` header (DNS rebinding). Flags: `--addr`, `--open`,
`--limit` (newest N entries, default 5000), `--detach`, `--stop`, `--idle`.

## Embedding model

The default is `bge-m3` (1024 dimensions, multilingual, via Ollama). On the real knowledge base (37 entries,
23 questions with a known target, 9 off-topic prompts) it ranked the target first for 22 of 23 questions
(`nomic-embed-text`: 13, `embeddinggemma-2`: 20, `qwen3-embedding:4b`: 20). The top models are within one or
two questions of each other, so that is not a statistically solid ranking, only a reason to stay with a good
default. Public benchmarks rank other models higher on German Wikipedia; test candidates on your own data.

Thresholds depend on the model. Each known model has a profile in `internal/config` (dimension, prefixes,
hook threshold, "similar" threshold); for other models set `EMBEDDING_DIM` and calibrate
(`KB_HOOK_DEBUG=1` prints the similarities) before overriding `KB_HOOK_MIN_SIM` / `KB_SIMILAR_MIN_SIM`.

The dimension and the model name are stored with the data, and the server refuses to start if they differ from
the configuration. To switch models on an existing database (make a backup first: copy the SQLite file while
Claude Code is closed, or `pg_dump` on Postgres):

```bash
ollama pull <new-model>
# set EMBEDDING_MODEL (and EMBEDDING_DIM for unknown models) in ~/.config/knowledge-base/config.env
"$KB" reindex
```

`reindex` re-embeds every chunk (also those of superseded entries) into a staging table, so an interrupted run
can simply be repeated, and then switches to the new vectors in one transaction (on Postgres it also rebuilds the
index). IDs and history are kept. Afterwards restart Claude Code. On Postgres, dimensions above 2000 are not
supported (they would need `halfvec`); SQLite has no limit. All users of a shared database must use the same model.

## Scaling

SQLite searches exactly over all entries in scope (see [Which database?](#which-database)); the time grows linearly
with the number of chunks. On Postgres, pgvector applies `WHERE` filters after the approximate HNSW scan, which silently drops results when a query is
limited to one of many projects. Measured on 150k chunks in 150 projects, this returned only 8.9 of 50 rows
(recall 0.18). Searches and similarity checks therefore select the rows of the project (or tag) first and sort
them exactly (4.9 ms, recall 1.0 in the same benchmark); only unfiltered searches use the HNSW index, with
iterative scans enabled. The same benchmark: HNSW search without filter 5 ms, brute force over all 150k rows
132 ms, index build 99 s (293 MB). Limits: a single project with far more than ~100k chunks would need its own
index (partial index or partitioning), and the HNSW index should fit in memory. `docker-compose.yml` therefore
sets `shared_buffers` and `maintenance_work_mem` to 512 MB and `shm_size` to 1 GB (parallel index builds fail
with Docker's 64 MB default); raise them on larger machines (see `config.env.example`).

## Notes

- **Switching the embedding model** needs `"$KB" reindex` (see [Embedding model](#embedding-model)).
- On macOS never copy a new binary over a running one with `cp`; the process is killed (exit 137) because the old code signature is cached. `scripts/kb` builds into a temporary file and renames it.
- The server works with any OpenAI-compatible `/embeddings` endpoint (Ollama, OpenAI, LM Studio, vLLM).
- Data lives in `~/.local/share/knowledge-base/knowledge.db` (with `-wal`/`-shm` files next to it while in use). Backup:
  copy the file while Claude Code is closed, or `sqlite3 knowledge.db ".backup backup.db"` at any time. With Postgres in
  Docker: the volume `knowledge-base_kb-pgdata`, backup with `docker exec knowledge-base-postgres pg_dump -U kb knowledge_base > backup.sql`.
- The Docker Postgres is bound to `127.0.0.1` only; `--postgres` gives it a random password.
- On Windows the plugin is not supported (the launcher is a bash script, and the background evaluation is not implemented).
- Tests: `go test ./...` (CI runs them on every push and pull request, against pgvector too). Both backends pass the same contract tests (`internal/store/store_contract_test.go`); SQLite always runs, Postgres only with a dedicated test database whose tables are dropped first: `KB_TEST_DATABASE_URL='postgres://kb:kb@localhost:5433/kb_test?sslmode=disable' go test ./internal/store` (create `kb_test` first).

## License

[MIT](LICENSE)
