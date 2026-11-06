# knowledge-base

Lokale Wissensdatenbank fuer Claude: ein MCP-Server in Go (stdio) auf Postgres + pgvector.
Hybrid-Suche aus Vektor- und Volltextsuche (Reciprocal Rank Fusion).

## Installation (macOS/Linux)

Voraussetzungen: Go (Version laut `go.mod`), Docker mit `docker compose`, [Ollama](https://ollama.com/download)
(oder ein OpenAI-Key) und optional Claude Code.

```bash
git clone <repo> && cd knowledge-base
./install.sh
```

Der Installer ist idempotent (erneutes Ausfuehren aktualisiert) und erledigt:

1. `.env` mit zufaelligem Postgres-Passwort anlegen (Rechte 600)
2. Postgres + pgvector per Docker starten
3. Embedding-Modell `nomic-embed-text` ueber Ollama ziehen
4. den MCP-Server nach `bin/` bauen und per `bin/knowledge-base-mcp --check` testen
5. nach Rueckfrage die Regeldatei nach `~/.claude/rules/` kopieren und den MCP bei Claude Code registrieren

Danach Claude Code neu starten. Der Server findet seine Konfiguration selbst in `.env`
(Variablen aus der Umgebung haben Vorrang), in `~/.claude.json` stehen keine Zugangsdaten.

| Option | Wirkung |
|---|---|
| `--yes` | keine Rueckfragen |
| `--no-claude` | Claude-Integration ueberspringen |
| `--no-ollama` | kein Modell ziehen |
| `--embedding openai` | OpenAI statt Ollama (`OPENAI_API_KEY` in der Umgebung) |
| `--database-url URL` | vorhandenes Postgres mit pgvector nutzen statt Docker |
| `--uninstall` | Registrierung und Regeldatei entfernen, Container stoppen (Daten bleiben) |
| `--uninstall --purge` | zusaetzlich Daten-Volume und `.env` loeschen |

### Gemeinsamer Server

Mehrere Personen koennen sich eine zentrale Datenbank teilen: Postgres mit pgvector auf einem Server
betreiben und bei jedem Nutzer `./install.sh --database-url postgres://...` ausfuehren. Alle muessen
dasselbe Embedding-Modell verwenden. Der MCP-Server selbst laeuft weiterhin lokal pro Nutzer (stdio).
Die Datenbank sollte dann nur ueber TLS und mit eigenem Passwort erreichbar sein.

### Manuelle Installation

```bash
cp .env.example .env && docker compose up -d
ollama pull nomic-embed-text
go build -o bin/knowledge-base-mcp ./cmd/knowledge-base-mcp
claude mcp add knowledge-base --scope user -- "$PWD/bin/knowledge-base-mcp"
cp claude/rules/knowledge-base.md ~/.claude/rules/
```

Anderes Embedding-Modell (z.B. OpenAI): `EMBEDDING_*` in `.env` anpassen, siehe `.env.example`.
Claude Desktop: gleicher Befehl als Eintrag in `claude_desktop_config.json`.

## Tools

| Tool | Zweck |
|---|---|
| `remember` | Kurzen Fakt merken. Liefert aehnliche bestehende Eintraege zurueck, damit Widersprueche erkannt werden. Mit `supersedes` ersetzt er alte Eintraege. |
| `supersede` | Eintraege als veraltet markieren (Korrektur). Sie werden nicht mehr gefunden, bleiben aber als Historie. |
| `search_knowledge` | Hybrid-Suche (nur aktive Eintraege), mit `project` = Projekt + projektuebergreifendes Wissen |
| `list_documents` | Eintraege auflisten, z.B. "was wurde zuletzt gelernt" (`include_superseded` zeigt auch Veraltetes) |
| `get_document` | Eintrag per ID lesen |
| `add_document` | Laengeres Dokument speichern. Gleiche `source` ersetzt das alte. |
| `delete_document` | Endgueltig loeschen (fuer Korrekturen `supersede` nutzen) |

Eintraege haben ein optionales `project` (leer = projektuebergreifend), einen `kind` (`fact`/`document`)
und einen `status` (`active`/`superseded`). Texte mit offensichtlichen Secrets (Keys, Tokens, Passwoerter)
werden abgelehnt.

## Automatisches Abrufen (Hooks)

Ein Claude-Code-Hook (`UserPromptSubmit`, `bin/knowledge-base-mcp hook`) speist zu einer konkreten Aufgabe
passendes Wissen automatisch in den Kontext ein: die bis zu 4 passendsten Eintraege zum Prompt
(Projekt + projektuebergreifend). Kurze Prompts (< 20 Zeichen) und Slash-Commands werden uebersprungen; was
in der Session schon eingespeist wurde, kommt nicht nochmal. Bewusst gibt es keinen Hook bei Sitzungsbeginn:
Wissen soll aufgabenbezogen abgerufen werden, nicht pauschal. Der Hook ist nur eine Absicherung; das eigentliche
Nachschlagen steuert die Regeldatei (Claude sucht selbst, wenn eine Aufgabe es erfordert).

Ein Hook stoert nie die Sitzung: bei Fehlern (Postgres aus, Ollama nicht erreichbar, Timeout 8 s) bleibt er
still und beendet sich mit Exit-Code 0. Ein Aufruf dauert typischerweise unter 100 ms.

```bash
bin/knowledge-base-mcp install-hooks     # in ~/.claude/settings.json eintragen (Backup: settings.json.kb-backup)
bin/knowledge-base-mcp uninstall-hooks   # nur die eigenen Eintraege entfernen
```

Der Installer macht das automatisch mit. Bestehende Hooks und die Reihenfolge der Schluessel bleiben erhalten;
nach Aenderungen Claude Code neu starten.

Relevanzfilter: Es werden nur Treffer mit Cosine-Aehnlichkeit >= 0.67 eingespeist (`KB_HOOK_MIN_SIM`).
Der Wert ist fuer `nomic-embed-text` kalibriert (themenfremde Prompts erreichen etwa 0.55-0.66, passende
0.67-0.77) und verpasst absichtlich lieber einen Treffer, als bei jedem Prompt Rauschen einzublenden.
Fehlersuche: `KB_HOOK_DEBUG=1` gibt alle Kandidaten mit Aehnlichkeit auf stderr aus, z.B.

```bash
echo '{"session_id":"t","cwd":"/pfad/zum/repo","hook_event_name":"UserPromptSubmit","prompt":"..."}' \
  | KB_HOOK_DEBUG=1 bin/knowledge-base-mcp hook
```

## Lernen am Sitzungsende (Sicherheitsnetz)

Zusaetzlich zum sofortigen `remember` waehrend der Sitzung wertet ein `SessionEnd`-Hook das Transkript aus:

1. Der Hook startet einen abgekoppelten Hintergrundprozess (`bin/knowledge-base-mcp extract`) und kehrt sofort zurueck,
   das Beenden der Sitzung wird nicht verzoegert.
2. Der Prozess liest nur Nachrichten von dir und Claudes Text (keine Tool-Ausgaben, keine Subagenten, keine
   Systemmeldungen). Sitzungen mit weniger als 3 Nutzer-Nachrichten werden uebersprungen, sehr lange auf 100.000
   Zeichen gekuerzt, wiederaufgenommene Sitzungen nur ab der zuletzt ausgewerteten Zeile.
3. Ein schlanker `claude -p`-Lauf (Modell `sonnet`, ohne Tools, MCP, Hooks und Settings) extrahiert Korrekturen,
   Praeferenzen/Entscheidungen und verifizierte, nicht aus dem Code ablesbare Fakten. Der Prompt ist bewusst
   streng: lieber 0-2 starke Fakten als viele schwache.
4. Jeder Fakt wird gegen die Knowledge Base geprueft: Aehnlichkeit >= 0.92 gilt als bekannt und wird verworfen,
   zwischen 0.78 und 0.92 entscheidet ein zweiter Modell-Aufruf `add`, `skip` oder `supersede` (nur mit IDs, die
   ihm vorgelegt wurden; im Zweifel `skip`). Secrets werden herausgefiltert.
5. Gespeicherte Eintraege tragen den Tag `auto`; Korrekturen zusaetzlich `korrektur`. Pruefen:
   `list_documents` mit `tag: auto`, oder `tail ~/Library/Caches/knowledge-base/extract.log` (Linux: `~/.cache/knowledge-base/`).

Die Auswertung nutzt dein Claude-Abo bzw. deinen API-Zugang (typisch wenige Cent je langer Sitzung); das
Transkript geht dabei an denselben Anbieter wie deine normale Sitzung. Ein Schutz verhindert, dass der
Hintergrundlauf selbst wieder Auswertungen oder Hooks ausloest.

| Einstellung (`.env` oder Umgebung) | Wirkung |
|---|---|
| `KB_AUTO_LEARN=0` | Auswertung am Sitzungsende ausschalten (Hook bleibt eingetragen, tut dann nichts) |
| `KB_EXTRACT_MODEL` | Modell fuer die Auswertung (Standard `sonnet`) |
| `KB_CLAUDE_BIN` | Pfad zur `claude`-CLI, falls nicht im `PATH` |

Manuell testen, ohne zu speichern:

```bash
bin/knowledge-base-mcp extract --transcript ~/.claude/projects/<projekt>/<session>.jsonl \
  --session test --project mein-projekt --dry-run
```

Nicht abgedeckt: Sitzungen, die hart beendet werden (Terminal geschlossen, Absturz), loesen evtl. kein
`SessionEnd` aus. Unter Windows ist die Hintergrundauswertung nicht implementiert.

## Automatisches Lernen

Das Verhalten steuert die Regeldatei `claude/rules/knowledge-base.md` (vom Installer nach `~/.claude/rules/` kopiert) (wird von Claude Code automatisch
geladen): bei Aufgaben erst suchen, bei Korrekturen/Entscheidungen/Erkenntnissen sofort `remember`,
veraltete Eintraege per `supersede` ersetzen.

## Hinweise

- **Embedding-Modell nicht nachtraeglich wechseln.** Die Vektordimension wird beim ersten Start im Schema fixiert; der Server bricht bei abweichendem `EMBEDDING_DIM` ab. Bei einem Modellwechsel Volume loeschen (`docker compose down -v`) und neu einlesen.
- Der Server nutzt jeden OpenAI-kompatiblen `/embeddings`-Endpunkt (Ollama, OpenAI, LM Studio, vLLM).
- Daten liegen im Docker-Volume `kb-pgdata`. Backup: `docker exec knowledge-base-postgres pg_dump -U kb knowledge_base > backup.sql`
- Postgres ist nur an `127.0.0.1` gebunden. Die Default-Zugangsdaten (`kb`/`kb`) sind nur fuer lokale Nutzung gedacht.
- Tests: `go test ./...`
