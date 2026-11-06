---
name: setup
description: Set up, repair or check the knowledge-base plugin backend (database, embedding model via Ollama, server binary), or move it from Postgres to SQLite. Use after installing the plugin, when the knowledge-base MCP server fails to connect, or when the user asks about the knowledge base installation.
argument-hint: "[--migrate-to-sqlite] [--database-url URL] [--postgres] [--embedding openai]"
---

Set up the backend of the knowledge-base plugin. Run every command below exactly as written with the Bash tool:
the paths are filled in for this installation.

1. Check the current state:

   ```bash
   CLAUDE_PLUGIN_DATA="${CLAUDE_PLUGIN_DATA}" "${CLAUDE_PLUGIN_ROOT}/scripts/setup.sh" --status
   ```

2. Explain the result to the user in a few lines. Before going on, ask about what you cannot decide alone:
   - **Database**: by default the knowledge base is a SQLite file (`~/.local/share/knowledge-base/knowledge.db`);
     nothing to install. Alternatives, only if the user wants them: `--database-url postgres://...` (an existing
     Postgres with pgvector, e.g. a server shared by a team) or `--postgres` (a local Postgres in Docker).
     An existing configuration keeps its database.
   - **Postgres in Docker in use** (status shows the container): offer `--migrate-to-sqlite`. It copies everything
     (IDs, history) into the SQLite file, switches the configuration and stops the container; the Docker volume
     stays as a backup. Docker is then no longer needed. Use it only after the user agrees.
   - **Missing prerequisites**: Ollama (local embeddings); without it `--embedding openai` (needs
     `OPENAI_API_KEY` in the environment). Go is only needed if no release binary exists for this version or
     platform. The user installs them; do not install system software yourself.
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
   lives in `~/.config/knowledge-base/config.env`. After `--migrate-to-sqlite` the reload is urgent (running
   sessions write into Postgres until then); pass on the script's hint about deleting the Docker volume later.

Never run `setup.sh --purge` unless the user explicitly asks to delete all knowledge: it deletes the SQLite file,
the Docker Postgres including its volume, and the configuration. Never delete the Docker volume yourself either.
