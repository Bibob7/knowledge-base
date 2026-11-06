---
name: viz
description: Open the live visualisation of the knowledge base (key figures, growth, knowledge map, filterable entry list) in the default browser, or stop it. Use when the user wants to see, browse or explore the knowledge base visually.
argument-hint: "[stop]"
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/scripts/kb --data ${CLAUDE_PLUGIN_DATA} viz *)
---

Run exactly one of these commands with the Bash tool, then reply with one short line (the URL, or that it stopped).
Do not run anything else and do not open the page yourself.

- If `$ARGUMENTS` is `stop`:

  ```bash
  ${CLAUDE_PLUGIN_ROOT}/scripts/kb --data ${CLAUDE_PLUGIN_DATA} viz --stop
  ```

- Otherwise (start, or reuse a running instance, and open the default browser):

  ```bash
  ${CLAUDE_PLUGIN_ROOT}/scripts/kb --data ${CLAUDE_PLUGIN_DATA} viz --detach --open
  ```

The page is read-only, listens on 127.0.0.1:8765 only, refreshes itself every 10 seconds and stops on its own
30 minutes after the last tab was closed. If the command fails, show its message; if the database is not
reachable, suggest `/knowledge-base:setup`.
