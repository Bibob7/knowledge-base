#!/usr/bin/env bash
# Installer for the knowledge base (macOS/Linux). Idempotent: running it again updates.
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
Usage: ./install.sh [options]

  --yes                 No questions asked
  --no-claude           Skip the Claude integration (rules file, MCP registration, hooks)
  --no-ollama           Do not pull an embedding model (e.g. if it is already available/configured differently)
  --database-url URL    Use an existing Postgres with pgvector instead of local Docker
                        (e.g. a central server for several users)
  --embedding openai    Use OpenAI instead of Ollama (expects OPENAI_API_KEY in the environment)
  --uninstall           Remove MCP registration, hooks and rules file, stop containers (data is kept)
  --purge               With --uninstall: also delete the data volume and .env
  -h, --help            This help
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --yes|-y) YES=1 ;;
    --no-claude) NO_CLAUDE=1 ;;
    --no-ollama) NO_OLLAMA=1 ;;
    --database-url) DATABASE_URL_ARG="${2:?--database-url needs a value}"; shift ;;
    --embedding) PROVIDER="${2:?--embedding needs a value}"; shift ;;
    --uninstall) UNINSTALL=1 ;;
    --purge) PURGE=1 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

say()  { printf '\033[1m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m!!\033[0m  %s\n' "$*" >&2; }
die()  { printf '\033[31mError:\033[0m %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

confirm() { # confirm "question" -> 0 on yes; --yes always answers yes
  [ "$YES" = 1 ] && return 0
  [ -t 0 ] || return 1
  local a; read -r -p "$1 [Y/n] " a
  case "${a:-y}" in y|Y|yes|Yes|YES) return 0 ;; *) return 1 ;; esac
}

# set_env KEY VALUE: replace or append a value in .env (portable, without sed -i)
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

# ---------- Uninstall ----------
if [ "$UNINSTALL" = 1 ]; then
  if [ -x "$BIN" ]; then
    say "Removing hooks from ~/.claude/settings.json"
    "$BIN" uninstall-hooks || warn "Could not remove the hooks"
  fi
  if have claude; then
    say "Removing MCP registration"
    claude mcp remove "$MCP_NAME" --scope user >/dev/null 2>&1 || warn "MCP was not registered"
  fi
  if [ -f "$RULES_DST" ]; then say "Removing $RULES_DST"; rm -f "$RULES_DST"; fi
  if have docker && [ -f docker-compose.yml ]; then
    if [ "$PURGE" = 1 ]; then
      say "Stopping containers and deleting the data volume"
      docker compose down -v
      rm -f .env
    else
      say "Stopping containers (data is kept; --purge deletes it)"
      docker compose down
    fi
  fi
  rm -f "$BIN"
  say "Uninstalled."
  exit 0
fi

# ---------- Prerequisites ----------
say "Checking prerequisites"
have go || die "Go is missing (see https://go.dev/dl/)."
need_go="$(awk '/^go [0-9]/{print $2; exit}' go.mod)"
have_go="$(go env GOVERSION | sed 's/^go//')"
lowest="$(printf '%s\n%s\n' "$need_go" "$have_go" | sort -V | head -n1)"
if [ "$lowest" != "$need_go" ]; then
  warn "Found Go $have_go, go.mod requires $need_go. Go may download the matching toolchain automatically."
fi

USE_DOCKER=1
[ -n "$DATABASE_URL_ARG" ] && USE_DOCKER=0
if [ "$USE_DOCKER" = 1 ]; then
  have docker || die "Docker is missing (or use --database-url with an existing Postgres)."
  docker compose version >/dev/null 2>&1 || die "'docker compose' (v2) is missing."
  docker info >/dev/null 2>&1 || die "The Docker daemon is not running."
fi

case "$PROVIDER" in
  ollama) ;;
  openai) [ -n "${OPENAI_API_KEY:-}" ] || die "--embedding openai needs OPENAI_API_KEY in the environment." ;;
  *) die "Unknown embedding provider: $PROVIDER (ollama|openai)" ;;
esac

# ---------- .env ----------
if [ ! -f .env ]; then
  say "Creating .env"
  cp .env.example .env
  chmod 600 .env
  if [ "$USE_DOCKER" = 1 ]; then
    pw="$(random_hex)"
    set_env POSTGRES_PASSWORD "$pw"
    set_env DATABASE_URL "postgres://$(get_env POSTGRES_USER):${pw}@localhost:$(get_env POSTGRES_PORT)/$(get_env POSTGRES_DB)?sslmode=disable"
  fi
else
  say ".env already exists, keeping it"
fi
if [ "$USE_DOCKER" = 0 ]; then
  set_env DATABASE_URL "$DATABASE_URL_ARG"
fi
if [ "$PROVIDER" = openai ]; then
  set_env EMBEDDING_BASE_URL "https://api.openai.com/v1"
  set_env EMBEDDING_MODEL "text-embedding-3-small"
  set_env EMBEDDING_DIM "1536"
  set_env EMBEDDING_DOC_PREFIX ""
  set_env EMBEDDING_QUERY_PREFIX ""
  set_env EMBEDDING_API_KEY "$OPENAI_API_KEY"
  NO_OLLAMA=1
fi

# ---------- Postgres ----------
if [ "$USE_DOCKER" = 1 ]; then
  say "Starting Postgres + pgvector (Docker)"
  docker compose up -d --wait
else
  say "Using the existing database (no Docker)"
fi

# ---------- Embedding model ----------
if [ "$NO_OLLAMA" = 0 ]; then
  model="$(get_env EMBEDDING_MODEL)"; model="${model:-bge-m3}"
  if have ollama; then
    if ollama list 2>/dev/null | awk 'NR>1{print $1}' | grep -qE "^${model}(:|$)"; then
      say "Ollama model $model is already available"
    else
      say "Pulling Ollama model $model"
      ollama pull "$model" || warn "Could not pull $model (is the Ollama server running?)"
    fi
  else
    warn "Ollama not found (https://ollama.com/download). Afterwards: ollama pull $model"
  fi
fi

# ---------- Build ----------
say "Building the MCP server"
mkdir -p bin
go build -o "$BIN" ./cmd/knowledge-base-mcp

say "Checking database and embedding"
if ! "$BIN" --check; then
  die "Self-test failed (see the message above). After fixing it, run ./install.sh again."
fi

# ---------- Claude integration ----------
if [ "$NO_CLAUDE" = 1 ]; then
  say "Claude integration skipped"
elif ! have claude; then
  warn "Claude Code CLI not found. Manually:"
  echo "    mkdir -p ~/.claude/rules && cp '$RULES_SRC' '$RULES_DST'"
  echo "    claude mcp add $MCP_NAME --scope user -- '$BIN'"
  echo "    '$BIN' install-hooks    # hooks: retrieval per prompt + learning at session end (~/.claude/settings.json)"
elif confirm "Copy the rules file to ~/.claude/rules/, register the MCP server and add hooks to ~/.claude/settings.json?"; then
  mkdir -p "$(dirname "$RULES_DST")"
  if [ -f "$RULES_DST" ] && ! cmp -s "$RULES_SRC" "$RULES_DST"; then
    cp "$RULES_DST" "$RULES_DST.bak"
    warn "Existing rules file backed up as $RULES_DST.bak"
  fi
  cp "$RULES_SRC" "$RULES_DST"
  claude mcp remove "$MCP_NAME" --scope user >/dev/null 2>&1 || true
  claude mcp add "$MCP_NAME" --scope user -- "$BIN"
  say "Adding hooks (retrieval per prompt, learning at session end) to ~/.claude/settings.json"
  "$BIN" install-hooks || warn "Could not add the hooks (later: $BIN install-hooks)"
  say "Registered. Restart Claude Code so that tools and hooks become active."
else
  say "Claude integration skipped"
fi

say "Done. Configuration: $ROOT/.env  |  Self-test: $BIN --check"
