---
name: setup
description: Set up, repair or check the knowledge-base plugin backend (Postgres/pgvector via Docker, embedding model via Ollama, server build). Use after installing the plugin, when the knowledge-base MCP server fails to connect, or when the user asks about the knowledge base installation.
argument-hint: "[--database-url URL] [--embedding openai]"
---

Set up the backend of the knowledge-base plugin. Run every command below exactly as written with the Bash tool:
the paths are filled in for this installation.

1. Check the current state:

   ```bash
   CLAUDE_PLUGIN_DATA="${CLAUDE_PLUGIN_DATA}" "${CLAUDE_PLUGIN_ROOT}/scripts/setup.sh" --status
   ```

2. Explain the result to the user in a few lines. Before going on, ask about what you cannot decide alone:
   - **Missing prerequisites**: Go (to build the server), Docker with a running daemon (local Postgres) and
     Ollama (local embeddings). The user installs them; do not install system software yourself. Without
     Docker they can pass `--database-url postgres://...` (an existing Postgres with pgvector, e.g. a shared
     team server); without Ollama `--embedding openai` (needs `OPENAI_API_KEY` in the environment).
   - **Pre-plugin installation found**: it runs alongside the plugin and would duplicate every hook and tool.
     Recommend `--remove-legacy` (it removes the old hooks, rules file and MCP registration and takes over
     the old configuration; the data stays). Use it only after the user agrees.
   - If `$ARGUMENTS` contains options, use them.

3. Run the setup with the agreed options (it is idempotent; the first build can take a minute):

   ```bash
   CLAUDE_PLUGIN_DATA="${CLAUDE_PLUGIN_DATA}" "${CLAUDE_PLUGIN_ROOT}/scripts/setup.sh" [options]
   ```

4. If it fails, read the message, fix what you can (e.g. start the setup again after the user started
   Docker or Ollama) and otherwise tell the user what is missing. If it succeeds, tell the user to run
   `/reload-plugins` (or restart Claude Code) so that the MCP server connects, and that the configuration
   lives in `~/.config/knowledge-base/config.env`.

Never run `setup.sh --purge` unless the user explicitly asks to delete all knowledge: it deletes the database.
