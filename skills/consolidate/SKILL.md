---
name: consolidate
description: Review the knowledge base for duplicates and overlapping entries (merge or supersede them after the user confirms) and list entries nobody used for a while. Use when the user wants to clean up, tidy, deduplicate or review the knowledge base, or asks whether it is getting cluttered.
argument-hint: "[project]"
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/scripts/kb --data ${CLAUDE_PLUGIN_DATA} consolidate *)
---

Consolidation runs in two steps, so that exactly what the user saw is applied.

1. Plan (changes nothing; one model call per 8 groups of similar entries, a few cents). If `$ARGUMENTS` names a
   project, add `--project <name>`:

   ```bash
   ${CLAUDE_PLUGIN_ROOT}/scripts/kb --data ${CLAUDE_PLUGIN_DATA} consolidate
   ```

2. Show the proposed changes to the user in a short list (per change: which entries, supersede or merge, the new
   text of a merge) and the list of unused entries. Ask whether to apply the changes. Do not apply without a clear yes.

3. Only after the user agreed:

   ```bash
   ${CLAUDE_PLUGIN_ROOT}/scripts/kb --data ${CLAUDE_PLUGIN_DATA} consolidate --apply
   ```

Unused entries are only a hint and are never changed automatically. If the user wants to act on one, use
`supersede` for outdated entries and `delete_document` only for junk. A merge can be undone with `delete_document`
on the new entry (the replaced entries become current again). If the command fails, show its message; if the
database is not reachable, suggest `/knowledge-base:setup`.
