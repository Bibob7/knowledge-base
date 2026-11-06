#!/usr/bin/env bash
# Installer fuer die knowledge-base (macOS/Linux). Idempotent: erneutes Ausfuehren aktualisiert.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

MCP_NAME="knowledge-base"
BIN="$ROOT/bin/knowledge-base-mcp"
RULES_SRC="$ROOT/claude/rules/knowledge-base.md"
RULES_DST="$HOME/.claude/rules/knowledge-base.md"

YES=0; NO_CLAUDE=0; NO_OLLAMA=0; UNINSTALL=0; PURGE=0
DATABASE_URL_ARG=""; PROVIDER="ollama"

usage() {
  cat <<USAGE
Verwendung: ./install.sh [Optionen]

  --yes                 Keine Rueckfragen
  --no-claude           Claude-Integration (Regeldatei, MCP-Registrierung, Hooks) ueberspringen
  --no-ollama           Kein Embedding-Modell ziehen (z.B. wenn es schon laeuft/anders konfiguriert ist)
  --database-url URL    Vorhandenes Postgres mit pgvector nutzen statt lokalem Docker
                        (z.B. zentraler Server fuer mehrere Nutzer)
  --embedding openai    OpenAI statt Ollama (erwartet OPENAI_API_KEY in der Umgebung)
  --uninstall           MCP-Registrierung, Hooks und Regeldatei entfernen, Container stoppen (Daten bleiben)
  --purge               Mit --uninstall: zusaetzlich Daten-Volume und .env loeschen
  -h, --help            Diese Hilfe
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --yes|-y) YES=1 ;;
    --no-claude) NO_CLAUDE=1 ;;
    --no-ollama) NO_OLLAMA=1 ;;
    --database-url) DATABASE_URL_ARG="${2:?--database-url braucht einen Wert}"; shift ;;
    --embedding) PROVIDER="${2:?--embedding braucht einen Wert}"; shift ;;
    --uninstall) UNINSTALL=1 ;;
    --purge) PURGE=1 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unbekannte Option: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

say()  { printf '\033[1m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m!!\033[0m  %s\n' "$*" >&2; }
die()  { printf '\033[31mFehler:\033[0m %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

confirm() { # confirm "Frage" -> 0 bei ja; --yes antwortet immer ja
  [ "$YES" = 1 ] && return 0
  [ -t 0 ] || return 1
  local a; read -r -p "$1 [J/n] " a
  case "${a:-j}" in j|J|y|Y|ja|yes) return 0 ;; *) return 1 ;; esac
}

# set_env KEY VALUE: Wert in .env ersetzen oder anhaengen (portabel, ohne sed -i)
set_env() {
  local key="$1" val="$2" tmp
  tmp="$(mktemp)"
  if grep -qE "^#?[[:space:]]*${key}=" .env 2>/dev/null; then
    awk -v k="$key" -v v="$val" 'BEGIN{done=0}
      $0 ~ "^#?[ \t]*"k"=" && !done {print k"="v; done=1; next} {print}' .env >"$tmp"
  else
    cat .env >"$tmp"; printf '%s=%s\n' "$key" "$val" >>"$tmp"
  fi
  cat "$tmp" >.env; rm -f "$tmp"
}
get_env() { grep -E "^${1}=" .env 2>/dev/null | tail -n1 | cut -d= -f2- || true; }

random_hex() {
  if have openssl; then openssl rand -hex 16; else LC_ALL=C tr -dc 'a-f0-9' </dev/urandom | head -c 32; fi
}

# ---------- Deinstallation ----------
if [ "$UNINSTALL" = 1 ]; then
  if [ -x "$BIN" ]; then
    say "Entferne Hooks aus ~/.claude/settings.json"
    "$BIN" uninstall-hooks || warn "Hooks konnten nicht entfernt werden"
  fi
  if have claude; then
    say "Entferne MCP-Registrierung"
    claude mcp remove "$MCP_NAME" --scope user >/dev/null 2>&1 || warn "MCP war nicht registriert"
  fi
  if [ -f "$RULES_DST" ]; then say "Entferne $RULES_DST"; rm -f "$RULES_DST"; fi
  if have docker && [ -f docker-compose.yml ]; then
    if [ "$PURGE" = 1 ]; then
      say "Stoppe Container und loesche Daten-Volume"
      docker compose down -v
      rm -f .env
    else
      say "Stoppe Container (Daten bleiben erhalten; --purge loescht sie)"
      docker compose down
    fi
  fi
  rm -f "$BIN"
  say "Deinstalliert."
  exit 0
fi

# ---------- Voraussetzungen ----------
say "Pruefe Voraussetzungen"
have go || die "Go fehlt (siehe https://go.dev/dl/)."
need_go="$(awk '/^go [0-9]/{print $2; exit}' go.mod)"
have_go="$(go env GOVERSION | sed 's/^go//')"
lowest="$(printf '%s\n%s\n' "$need_go" "$have_go" | sort -V | head -n1)"
if [ "$lowest" != "$need_go" ]; then
  warn "Go $have_go gefunden, go.mod verlangt $need_go. Go laedt ggf. automatisch die passende Toolchain."
fi

USE_DOCKER=1
[ -n "$DATABASE_URL_ARG" ] && USE_DOCKER=0
if [ "$USE_DOCKER" = 1 ]; then
  have docker || die "Docker fehlt (oder nutze --database-url mit einem vorhandenen Postgres)."
  docker compose version >/dev/null 2>&1 || die "'docker compose' (v2) fehlt."
  docker info >/dev/null 2>&1 || die "Docker-Daemon laeuft nicht."
fi

case "$PROVIDER" in
  ollama) ;;
  openai) [ -n "${OPENAI_API_KEY:-}" ] || die "--embedding openai braucht OPENAI_API_KEY in der Umgebung." ;;
  *) die "Unbekannter Embedding-Provider: $PROVIDER (ollama|openai)" ;;
esac

# ---------- .env ----------
if [ ! -f .env ]; then
  say "Erzeuge .env"
  cp .env.example .env
  chmod 600 .env
  if [ "$USE_DOCKER" = 1 ]; then
    pw="$(random_hex)"
    set_env POSTGRES_PASSWORD "$pw"
    set_env DATABASE_URL "postgres://$(get_env POSTGRES_USER):${pw}@localhost:$(get_env POSTGRES_PORT)/$(get_env POSTGRES_DB)?sslmode=disable"
  fi
else
  say ".env existiert bereits, wird beibehalten"
fi
if [ "$USE_DOCKER" = 0 ]; then
  set_env DATABASE_URL "$DATABASE_URL_ARG"
fi
if [ "$PROVIDER" = openai ]; then
  set_env EMBEDDING_BASE_URL "https://api.openai.com/v1"
  set_env EMBEDDING_MODEL "text-embedding-3-small"
  set_env EMBEDDING_DIM "1536"
  set_env EMBEDDING_API_KEY "$OPENAI_API_KEY"
  NO_OLLAMA=1
fi

# ---------- Postgres ----------
if [ "$USE_DOCKER" = 1 ]; then
  say "Starte Postgres + pgvector (Docker)"
  docker compose up -d --wait
else
  say "Nutze vorhandene Datenbank (kein Docker)"
fi

# ---------- Embedding-Modell ----------
if [ "$NO_OLLAMA" = 0 ]; then
  model="$(get_env EMBEDDING_MODEL)"; model="${model:-nomic-embed-text}"
  if have ollama; then
    if ollama list 2>/dev/null | awk 'NR>1{print $1}' | grep -qE "^${model}(:|$)"; then
      say "Ollama-Modell $model ist bereits vorhanden"
    else
      say "Ziehe Ollama-Modell $model"
      ollama pull "$model" || warn "Konnte $model nicht ziehen (laeuft der Ollama-Server?)"
    fi
  else
    warn "Ollama nicht gefunden (https://ollama.com/download). Danach: ollama pull $model"
  fi
fi

# ---------- Build ----------
say "Baue MCP-Server"
mkdir -p bin
go build -o "$BIN" ./cmd/knowledge-base-mcp

say "Pruefe Datenbank und Embedding"
if ! "$BIN" --check; then
  die "Selbsttest fehlgeschlagen (siehe Meldung oben). Nach der Korrektur ./install.sh erneut ausfuehren."
fi

# ---------- Claude-Integration ----------
if [ "$NO_CLAUDE" = 1 ]; then
  say "Claude-Integration uebersprungen"
elif ! have claude; then
  warn "Claude Code CLI nicht gefunden. Manuell:"
  echo "    mkdir -p ~/.claude/rules && cp '$RULES_SRC' '$RULES_DST'"
  echo "    claude mcp add $MCP_NAME --scope user -- '$BIN'"
  echo "    '$BIN' install-hooks    # Hooks: Abruf pro Prompt + Lernen am Sitzungsende (~/.claude/settings.json)"
elif confirm "Regeldatei nach ~/.claude/rules/ kopieren, MCP registrieren und Hooks in ~/.claude/settings.json eintragen?"; then
  mkdir -p "$(dirname "$RULES_DST")"
  if [ -f "$RULES_DST" ] && ! cmp -s "$RULES_SRC" "$RULES_DST"; then
    cp "$RULES_DST" "$RULES_DST.bak"
    warn "Bestehende Regeldatei gesichert als $RULES_DST.bak"
  fi
  cp "$RULES_SRC" "$RULES_DST"
  claude mcp remove "$MCP_NAME" --scope user >/dev/null 2>&1 || true
  claude mcp add "$MCP_NAME" --scope user -- "$BIN"
  say "Trage Hooks (Abruf pro Prompt, Lernen am Sitzungsende) in ~/.claude/settings.json ein"
  "$BIN" install-hooks || warn "Hooks konnten nicht eingetragen werden (spaeter: $BIN install-hooks)"
  say "Registriert. Claude Code neu starten, damit Tools und Hooks aktiv werden."
else
  say "Claude-Integration uebersprungen"
fi

say "Fertig. Konfiguration: $ROOT/.env  |  Selbsttest: $BIN --check"
