# Operations

Installation details, configuration, databases and commands. How the parts work is in
[How it works](how-it-works.md).

- [Installation](#installation)
- [Which database?](#which-database)
- [Command line](#command-line)
- [Project names](#project-names)
- [Retrieval per prompt](#retrieval-per-prompt)
- [Learning at session end](#learning-at-session-end)
- [Visualisation](#visualisation)
- [Consolidation](#consolidation)
- [Usage report](#usage-report)
- [Embedding model](#embedding-model)
- [Scaling](#scaling)
- [Backups and notes](#backups-and-notes)

## Installation

Prerequisite: [Ollama](https://ollama.com/download) for local embeddings (without it: OpenAI embeddings with
`--embedding openai`). SQLite is built into the server binary, so no database server or Docker is needed. The
binary is downloaded from the [GitHub release](https://github.com/Bibob7/knowledge-base/releases) of the plugin
version (macOS and Linux, amd64 and arm64); [Go](https://go.dev/dl/) 1.27 or newer is only needed where no
release binary exists.

```
/plugin marketplace add Bibob7/knowledge-base
/plugin install knowledge-base@bibob7
/knowledge-base:setup
```

`/knowledge-base:setup` lets Claude check the prerequisites, explain what is missing and run
`scripts/setup.sh`. The script is idempotent (running it again repairs and updates):

1. creates `~/.config/knowledge-base/config.env` (mode 600),
2. pulls the embedding model `bge-m3` via Ollama,
3. installs the server binary into the plugin data directory,
4. creates the database `~/.local/share/knowledge-base/knowledge.db` (mode 600) and runs the self-test (`--check`).

Then run `/reload-plugins` or restart Claude Code.

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

Uninstall: `/plugin uninstall knowledge-base@bibob7`. The database is kept; `scripts/setup.sh --purge` deletes it.

### The launcher

`scripts/kb` is the launcher used by the MCP server and the hooks. It installs `knowledge-base-mcp` once per plugin
version into `${CLAUDE_PLUGIN_DATA}/bin/` (`~/.claude/plugins/data/knowledge-base-bibob7/bin/`) and runs it. It
downloads the release binary and checks it against the release's `checksums.txt`; if there is none for the version
or platform, it builds from source with Go. A hook never installs: until the binary exists, hooks do nothing. The
configuration and the database live outside the plugin and survive updates and uninstalling it.

### Upgrading from the pre-plugin installation

Older versions were installed with `install.sh`: hooks in `~/.claude/settings.json`, the rules file
`~/.claude/rules/knowledge-base.md` and an MCP server registered with `claude mcp add`. Running both would
duplicate every hook and tool. `/knowledge-base:setup` detects it; `setup.sh --remove-legacy` removes it and takes
over its `.env` if there is one. The data volume is the same, so all entries are kept.

### Moving from Postgres in Docker to SQLite

Up to version 0.5 the knowledge base ran in Postgres in Docker. Such an installation keeps working after the
update. To drop Docker, run `/knowledge-base:setup --migrate-to-sqlite`:

- It copies every entry with its ID, history, tags, timestamps and embeddings into the SQLite file and checks the
  counts.
- It switches `DATABASE_URL`; the old URL stays in the configuration as a comment.
- It stops the container.

Run `/reload-plugins` right after it (until then running sessions still write into Postgres). The Docker volume
stays as a backup; once everything works, delete it with `docker volume rm knowledge-base_kb-pgdata`. The command
`"$KB" migrate --from URL --to URL` copies between any two databases, in both directions.

## Which database?

| | SQLite (default) | Postgres + pgvector |
|---|---|---|
| **For** | a personal knowledge base | team servers, very many entries |
| **Setup** | one file, built into the program | own server or Docker |
| **Vector search** | exact: compares with every vector | HNSW index: fast, approximate |
| **Good up to** | about 50,000 chunks | far beyond that |
| **Concurrency** | several sessions thanks to WAL | many users at once |

SQLite compares the query with every stored vector exactly, plus FTS5 full text. Opening the file and searching
10,000 chunks with 1024 dimensions, as the hook does per prompt, takes about 80 ms on an M1 Pro. Parallel sessions,
hooks and the evaluation share the file (WAL mode). Both backends pass the same contract tests.

### Shared server

Several people can share a central database: run Postgres with pgvector on a server and run
`/knowledge-base:setup --database-url postgres://...` for each user. An existing personal knowledge base can be
copied there with `"$KB" migrate --to postgres://...`. Everyone must use the same embedding model. The MCP server
itself still runs locally per user (stdio). The database should only be reachable over TLS and with its own
password.

## Command line

The server binary also has helper commands (`viz`, `reindex`, `migrate`, `extract`, `sweep`). It finds the
configuration in `~/.config/knowledge-base/config.env` on its own; variables from the environment take precedence,
and `KB_ENV_FILE` points to another file.

```bash
KB="$(ls ~/.claude/plugins/data/knowledge-base-bibob7/bin/knowledge-base-mcp-* | tail -n1)"
"$KB" --check
```

## Project names

The project is the lowercase folder name of the **main repository**. A git worktree, such as the ones Claude Code
creates for agents under `.claude/worktrees/`, belongs to the repository it was created from, so it gets that
project's knowledge. The hooks, the evaluation at session end and the MCP server all resolve the name the same
way. The server names it in its instructions, so Claude does not have to derive it itself.

- Two repositories with the same folder name, or a renamed folder: put the name to use into a file
  `.knowledge-base-project` in the repository root (first line; it applies to its worktrees too).
- `KB_PROJECT` in the environment overrides the name for the hook and the MCP server.
- Outside a git repository there is no project. The evaluation at session end then only keeps cross-project facts,
  so project-specific ones never end up cross-project and shown everywhere.

## Retrieval per prompt

The hook (`"$KB" hook`) injects hits above `KB_HOOK_MIN_SIM` (0.52 for `bge-m3`, see
[Embedding model](#embedding-model)). For troubleshooting, `KB_HOOK_DEBUG=1` prints all candidates with their
similarity to stderr:

```bash
echo '{"session_id":"t","cwd":"/path/to/repo","hook_event_name":"UserPromptSubmit","prompt":"..."}' \
  | KB_HOOK_DEBUG=1 "$KB" hook
```

## Learning at session end

| Setting (`config.env` or environment) | Effect |
|---|---|
| `KB_AUTO_LEARN=0` | disable the evaluation at session end (the hook stays registered but does nothing) |
| `KB_EXTRACT_MODEL` | model used for the evaluation (default `sonnet`) |
| `KB_CLAUDE_BIN` | path to the `claude` CLI if it is not on the `PATH` |
| `KB_SWEEP_IDLE` / `KB_SWEEP_MAX_AGE` | idle time before a transcript is swept (default `30m`) / maximum age (default `168h`) |
| `KB_CACHE_DIR` | directory for the log and the progress files (default: user cache dir, e.g. for tests) |

Review what was learned: `list_documents` with `tag: auto`, or
`tail ~/Library/Caches/knowledge-base/extract.log` (Linux: `~/.cache/knowledge-base/`).

Test manually without storing anything:

```bash
"$KB" extract --transcript ~/.claude/projects/<project>/<session>.jsonl \
  --session test --project my-project --dry-run
"$KB" sweep --dry-run --force    # what a sweep would evaluate
```

On Windows the background evaluation is not implemented.

## Visualisation

In Claude Code: `/knowledge-base:viz` opens it in the default browser (`/knowledge-base:viz stop` stops it). From
the shell:

```bash
"$KB" viz --open             # foreground, Ctrl+C stops it; http://127.0.0.1:8765/
"$KB" viz --detach --open    # background (reuses a running instance), what the skill runs
"$KB" viz --stop             # stop the background instance
```

A live, read-only view of the database: key figures, growth over time, entries per project, a **knowledge map**
(entries placed by semantic similarity via a PCA of the embeddings; hollow points are outdated, dashed lines lead
to the entry that replaced them) and a filterable list with the full text. The page refreshes itself every 10
seconds and needs no internet access (no external scripts or fonts).

In the background the server stops on its own 30 minutes after the last request (`--idle`); an open tab polls
every 10 seconds, background tabs at least once a minute, so it only stops after the tab was closed. The pid file
and log are in the cache directory (`viz.pid`, `viz.log`). There is no authentication, so the server only listens
on loopback addresses and rejects requests with a foreign `Host` header (DNS rebinding). Flags: `--addr`,
`--open`, `--limit` (newest N entries, default 5000), `--detach`, `--stop`, `--idle`.

## Consolidation

In Claude Code: `/knowledge-base:consolidate` (optionally with a project name). It plans, shows the proposed
changes, and applies them only after you agree. From the shell:

```bash
"$KB" consolidate                       # plan: print and save the proposed changes, change nothing
"$KB" consolidate --project my-project  # only that project's entries (plus global ones)
"$KB" consolidate --apply               # apply exactly the saved plan
```

| Flag | Effect |
|---|---|
| `--min-sim` | similarity from which entries are grouped (default: the model's "similar" threshold, 0.70 for `bge-m3`) |
| `--max-groups` | review at most this many groups per run (default 40; one model call per 8 groups) |
| `--unused-days` | also list entries neither injected nor retrieved for this many days (default 30, 0 = off) |

The plan is saved as `consolidate-plan.json` in the cache directory and is valid for a day. The model calls use
the same `claude -p` setup as the evaluation at session end (`KB_EXTRACT_MODEL`, `KB_CLAUDE_BIN`). How it works:
[Keeping the memory lean](how-it-works.md#keeping-the-memory-lean).

Usage counters (how often an entry was injected or retrieved) are stored in the table `doc_usage`. `migrate` does
not copy them; they start again in the target database.

## Usage report

`scripts/usage-report.py` shows whether the knowledge base pays off with your own data: how many entries there
are, how many are cross-project, how often the prompt hook injected something, which entries were injected most,
and how much auto memory Claude Code keeps per repository for comparison. It is read-only and uses the SQLite
database and the transcripts in `~/.claude/projects/`.

```bash
scripts/usage-report.py            # since the first entry
scripts/usage-report.py --days 14  # the last two weeks
```

A prompt without a hit leaves no trace in the transcript, so "prompts with an injection" is a lower bound.
Whether an injected entry actually helped cannot be measured this way.

## Embedding model

The default is `bge-m3` (1024 dimensions, multilingual, via Ollama). On the real knowledge base (37 entries, 23
questions with a known target, 9 off-topic prompts) it ranked the target first for 22 of 23 questions
(`nomic-embed-text`: 13, `embeddinggemma-2`: 20, `qwen3-embedding:4b`: 20). The top models are within one or two
questions of each other, so that is not a statistically solid ranking, only a reason to stay with a good default.
Public benchmarks rank other models higher on German Wikipedia; test candidates on your own data.

Thresholds depend on the model. Each known model has a profile in `internal/config` (dimension, prefixes, hook
threshold, "similar" threshold). For other models set `EMBEDDING_DIM` and calibrate (`KB_HOOK_DEBUG=1` prints the
similarities) before overriding `KB_HOOK_MIN_SIM` / `KB_SIMILAR_MIN_SIM`. The server works with any
OpenAI-compatible `/embeddings` endpoint (Ollama, OpenAI, LM Studio, vLLM).

The dimension and the model name are stored with the data, and the server refuses to start if they differ from the
configuration. To switch models on an existing database (make a [backup](#backups-and-notes) first):

```bash
ollama pull <new-model>
# set EMBEDDING_MODEL (and EMBEDDING_DIM for unknown models) in ~/.config/knowledge-base/config.env
"$KB" reindex
```

`reindex` re-embeds every chunk (also those of superseded entries) into a staging table, so an interrupted run can
simply be repeated, and then switches to the new vectors in one transaction (on Postgres it also rebuilds the
index). IDs and history are kept. Afterwards restart Claude Code. On Postgres, dimensions above 2000 are not
supported (they would need `halfvec`); SQLite has no limit. All users of a shared database must use the same model.

## Scaling

SQLite searches exactly over all entries in scope; the time grows linearly with the number of chunks.

On Postgres, pgvector applies `WHERE` filters after the approximate HNSW scan, which silently drops results when a
query is limited to one of many projects. Measured on 150k chunks in 150 projects, this returned only 8.9 of 50
rows (recall 0.18). Searches and similarity checks therefore select the rows of the project (or tag) first and sort
them exactly (4.9 ms, recall 1.0 in the same benchmark); only unfiltered searches use the HNSW index, with
iterative scans enabled. The same benchmark: HNSW search without filter 5 ms, brute force over all 150k rows
132 ms, index build 99 s (293 MB).

Limits: a single project with far more than ~100k chunks would need its own index (partial index or
partitioning), and the HNSW index should fit in memory. `docker-compose.yml` therefore sets `shared_buffers` and
`maintenance_work_mem` to 512 MB and `shm_size` to 1 GB (parallel index builds fail with Docker's 64 MB default);
raise them on larger machines (see `config.env.example`).

## Backups and notes

- Data lives in `~/.local/share/knowledge-base/knowledge.db` (with `-wal`/`-shm` files next to it while in use).
  Backup: copy the file while Claude Code is closed, or run `sqlite3 knowledge.db ".backup backup.db"` at any time.
  With Postgres in Docker: the volume `knowledge-base_kb-pgdata`, backup with
  `docker exec knowledge-base-postgres pg_dump -U kb knowledge_base > backup.sql`.
- The Docker Postgres is bound to `127.0.0.1` only; `--postgres` gives it a random password.
- On macOS never copy a new binary over a running one with `cp`: the process is killed (exit 137) because the old
  code signature is cached. `scripts/kb` builds into a temporary file and renames it.
- Windows is not supported (the launcher is a bash script, and the background evaluation is not implemented).
