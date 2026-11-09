# Comparison with other memory solutions

> As of November 2026. The information about other projects comes from their project pages and from blog
> posts, partly written by the vendors themselves. It was not tested by us, and these projects change fast.

## Overview

| Solution | What is stored? | When? | Where? |
|---|---|---|---|
| [claude-mem](https://github.com/thedotmack/claude-mem) | observations of all tool activity, condensed by an AI | hooks; index at session start | local: SQLite + Chroma; optional cloud sync |
| Mem0, Supermemory | memories from conversations | MCP tools or plugin hooks | mostly a hosted service |
| Basic Memory | notes as Markdown files | MCP tools, Claude decides | local files |
| MCP reference server "memory" | knowledge graph of entities and relations | MCP tools, Claude decides | local JSON file |
| **Knowledge base** | curated facts: corrections, decisions | tools, hook per prompt, review at session end | local SQLite file or team Postgres |

The basic pattern: either a hook records everything, or the model has to remember to remember. The knowledge
base combines both, but filters strictly.

Gaps: for Basic Memory and the MCP reference server, "Claude decides" is inferred from both being plain MCP
servers; we did not check their documentation in detail.

## claude-mem: a diary, not a reference

claude-mem is the closest relative. Both work automatically through hooks, locally, and with a hybrid of
keyword search (SQLite FTS5) and vector search. The difference is the philosophy.

| | claude-mem | Knowledge base |
|---|---|---|
| **Question it answers** | What did I do here? | What holds, and what helps right now? |
| **Recorded** | every tool action, condensed by an AI | only corrections, decisions, preferences |
| **Injected** | index at start, details via search | up to 4 hits per prompt |
| **Outdated entries** | not described in the README | replaced via `supersede`, with history |
| **Scope** | per project | project plus cross-project |
| **Runtime** | local worker service (Bun) with web viewer | no always-on process |
| **Team** | optional via cmem.ai (cloud sync, TeamBrain) | your own Postgres database |

Source: [claude-mem README](https://github.com/thedotmack/claude-mem), v15.12.1. Both approaches have their place.
If you want the most complete record of past work, claude-mem fits; if you want little but reliable knowledge
that corrects itself, the knowledge base does.

## What the knowledge base does differently

- **Only what matters.** Corrections, decisions and verified facts, not every tool action.
- **Relevant, not wholesale.** Per prompt the up to 4 most relevant notes; nothing is preloaded at start.
- **Across projects.** General knowledge applies everywhere, project knowledge only in its repository.
- **Replace with history.** Contradictions surface when storing; outdated entries stay traceable.
- **Double safety net.** Claude remembers right away; what slips through, the review at session end catches.
- **Local and simple.** One SQLite file, local embeddings, no cloud service, no always-on process.

The price: Ollama as a prerequisite, and the review at session end costs a few cents per longer session through
your own Claude plan.

## Sources

- [Claude Code docs: How Claude remembers your project](https://code.claude.com/docs/en/memory) (vendor documentation)
- [Claude Help Center: memory](https://support.claude.com/en/articles/11817273) (vendor documentation)
- [claude-mem on GitHub](https://github.com/thedotmack/claude-mem) (project page)
- [Best Claude Code Memory Plugins & Tools in 2026](https://brain-tube.com/compare/claude-code-memory-tools) (blog)
- [ClaudeMem vs Context Mode](https://www.mindstudio.ai/blog/claudemem-vs-context-mode-claude-code-memory-plugins) (blog)
- [Claude Code Memory, Explained](https://blog.memoryplugin.com/claude-code-memory/) (blog of a vendor)
