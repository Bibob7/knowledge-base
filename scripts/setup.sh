#!/usr/bin/env bash
# Sets up the backend of the knowledge-base plugin (macOS/Linux). Idempotent: running it again
# repairs and updates. Called by the /knowledge-base:setup skill; can also be run by hand.
#
#   setup.sh --status                 only report what is installed and what is missing
#   setup.sh [options]                set up: config, Postgres (Docker), embedding model, build, self-test
#
# Options:
#   --database-url URL    use an existing Postgres with pgvector instead of local Docker (shared server)
#   --embedding openai    OpenAI embeddings instead of Ollama (expects OPENAI_API_KEY in the environment)
#   --no-ollama           do not pull the embedding model
#   --import-env PATH     take over the configuration of an earlier installation (its .env)
#   --remove-legacy       remove the integration of the pre-plugin installation (hooks in
#                         ~/.claude/settings.json, ~/.claude/rules/knowledge-base.md, user-scope MCP server)
#   --purge               stop Postgres and DELETE the data volume and the configuration
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KB="$ROOT/scripts/kb"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/knowledge-base"
CONFIG="$CONFIG_DIR/config.env"
SETTINGS="$HOME/.claude/settings.json"
LEGACY_RULES="$HOME/.claude/rules/knowledge-base.md"
COMPOSE=(docker compose -p knowledge-base -f "$ROOT/docker-compose.yml" --env-file "$CONFIG")

STATUS=0; NO_OLLAMA=0; REMOVE_LEGACY=0; PURGE=0
DATABASE_URL_ARG=""; PROVIDER="ollama"; IMPORT_ENV=""

usage() { sed -n '2,17p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --status) STATUS=1 ;;
    --database-url) DATABASE_URL_ARG="${2:?--database-url needs a value}"; shift ;;
    --embedding) PROVIDER="${2:?--embedding needs a value}"; shift ;;
    --no-ollama) NO_OLLAMA=1 ;;
    --import-env) IMPORT_ENV="${2:?--import-env needs a path}"; shift ;;
    --remove-legacy) REMOVE_LEGACY=1 ;;
    --purge) PURGE=1 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

say()  { printf '==> %s\n' "$*"; }
ok()   { printf '  ok   %s\n' "$*"; }
miss() { printf '  --   %s\n' "$*"; }
warn() { printf '!!  %s\n' "$*" >&2; }
die()  { printf 'Error: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

set_env() { # set_env KEY VALUE: replace or append in $CONFIG (portable, without sed -i)
  local key="$1" val="$2" tmp
  tmp="$(mktemp)"
  if grep -qE "^#?[[:space:]]*${key}=" "$CONFIG" 2>/dev/null; then
    awk -v k="$key" -v v="$val" 'BEGIN{done=0}
      $0 ~ "^#?[ \t]*"k"=" && !done {print k"="v; done=1; next} {print}' "$CONFIG" >"$tmp"
  else
    cat "$CONFIG" >"$tmp"; printf '%s=%s\n' "$key" "$val" >>"$tmp"
  fi
  cat "$tmp" >"$CONFIG"; rm -f "$tmp"
}
get_env() { grep -E "^${1}=" "$CONFIG" 2>/dev/null | tail -n1 | cut -d= -f2- || true; }
random_hex() {
  if have openssl; then openssl rand -hex 16; else LC_ALL=C tr -dc 'a-f0-9' </dev/urandom | head -c 32; fi
}

# The pre-plugin installation registered "<repo>/bin/knowledge-base-mcp hook" in settings.json;
# its configuration is <repo>/.env.
legacy_hook_bin() {
  [ -f "$SETTINGS" ] || return 1
  grep -oE '"[^"]*knowledge-base-mcp'"'"'? hook"' "$SETTINGS" | head -n1 | sed -E "s/^\"'?//; s/'? hook\"$//"
}
legacy_env() {
  local bin; bin="$(legacy_hook_bin)" || return 1
  [ -n "$bin" ] && [ -f "$(dirname "$bin")/../.env" ] && (cd "$(dirname "$bin")/.." && echo "$PWD/.env")
}
legacy_mcp() { have claude && claude mcp get knowledge-base >/dev/null 2>&1; }

# ---------- Status ----------
status() {
  local bin
  say "Status"
  if [ -f "$CONFIG" ]; then ok "configuration $CONFIG"; else miss "configuration $CONFIG (missing)"; fi
  if have go; then ok "Go $(go env GOVERSION)"; else ok "Go not installed (only needed without a release binary)"; fi
  bin="$("$KB" --path)"
  if [ -n "$bin" ] && [ -x "$bin" ]; then ok "server binary $bin"; else miss "server binary (not built)"; fi
  if have docker && docker info >/dev/null 2>&1; then
    if [ "$(docker inspect -f '{{.State.Health.Status}}' knowledge-base-postgres 2>/dev/null)" = healthy ]; then
      ok "Postgres container knowledge-base-postgres (healthy)"
    else miss "Postgres container knowledge-base-postgres (not running)"; fi
  else miss "Docker (not installed or daemon not running; not needed with --database-url)"; fi
  if have ollama; then ok "Ollama"; else miss "Ollama (https://ollama.com/download; not needed with --embedding openai)"; fi
  if [ -f "$CONFIG" ] && [ -n "$bin" ] && [ -x "$bin" ]; then
    if KB_ENV_FILE="$CONFIG" "$bin" --check >/dev/null 2>&1; then ok "self-test (database + embedding)"
    else miss "self-test failed: KB_ENV_FILE='$CONFIG' '$bin' --check"; fi
  fi
  say "Pre-plugin installation"
  local found=0
  if legacy_hook_bin >/dev/null; then miss "hooks in $SETTINGS ($(legacy_hook_bin))"; found=1; fi
  if [ -f "$LEGACY_RULES" ]; then miss "rules file $LEGACY_RULES"; found=1; fi
  if legacy_mcp; then miss "MCP server 'knowledge-base' registered with 'claude mcp add'"; found=1; fi
  if legacy_env >/dev/null; then echo "       its configuration: $(legacy_env)"; fi
  if [ "$found" = 1 ]; then
    echo "       It runs alongside the plugin (duplicate hooks and tools): remove it with --remove-legacy."
  else ok "none found"; fi
}

if [ "$STATUS" = 1 ]; then status; exit 0; fi

# ---------- Purge ----------
if [ "$PURGE" = 1 ]; then
  if have docker && [ -f "$CONFIG" ]; then
    say "Stopping Postgres and deleting the data volume"
    "${COMPOSE[@]}" down -v
  fi
  rm -f "$CONFIG"
  say "Purged. Uninstall the plugin with: /plugin uninstall knowledge-base"
  exit 0
fi

# ---------- Remove pre-plugin integration ----------
if [ "$REMOVE_LEGACY" = 1 ]; then
  if legacy_hook_bin >/dev/null; then
    [ -z "$IMPORT_ENV" ] && [ ! -f "$CONFIG" ] && IMPORT_ENV="$(legacy_env || true)"
    say "Removing old hooks from $SETTINGS (backup: $SETTINGS.kb-backup)"
    bin="$("$KB" --ensure)"
    "$bin" uninstall-hooks
  fi
  if [ -f "$LEGACY_RULES" ]; then say "Removing $LEGACY_RULES"; rm -f "$LEGACY_RULES"; fi
  if legacy_mcp; then
    say "Removing the old MCP server registration"
    claude mcp remove knowledge-base --scope user >/dev/null 2>&1 || claude mcp remove knowledge-base >/dev/null 2>&1 || warn "could not remove it: claude mcp remove knowledge-base"
  fi
fi

# ---------- Prerequisites ----------
USE_DOCKER=1
[ -n "$DATABASE_URL_ARG" ] && USE_DOCKER=0
[ "$USE_DOCKER" = 1 ] && [ -f "$CONFIG" ] && [ -n "$(get_env DATABASE_URL)" ] \
  && ! grep -qE '@(localhost|127\.0\.0\.1):' <<<"$(get_env DATABASE_URL)" && USE_DOCKER=0 # configured remote DB
say "Checking prerequisites"
if [ "$USE_DOCKER" = 1 ]; then
  have docker || die "Docker is missing (or pass --database-url with an existing Postgres + pgvector)."
  docker compose version >/dev/null 2>&1 || die "'docker compose' (v2) is missing."
  docker info >/dev/null 2>&1 || die "The Docker daemon is not running."
fi
case "$PROVIDER" in
  ollama) ;;
  openai) [ -n "${OPENAI_API_KEY:-}" ] || die "--embedding openai needs OPENAI_API_KEY in the environment." ;;
  *) die "Unknown embedding provider: $PROVIDER (ollama|openai)" ;;
esac

# ---------- Configuration ----------
mkdir -p "$CONFIG_DIR"; chmod 700 "$CONFIG_DIR"
if [ -n "$IMPORT_ENV" ]; then
  [ -f "$IMPORT_ENV" ] || die "--import-env: $IMPORT_ENV not found"
  if [ -f "$CONFIG" ] && ! cmp -s "$IMPORT_ENV" "$CONFIG"; then
    cp "$CONFIG" "$CONFIG.bak"; warn "existing configuration backed up as $CONFIG.bak"
  fi
  say "Taking over the configuration from $IMPORT_ENV"
  cp "$IMPORT_ENV" "$CONFIG"
elif [ ! -f "$CONFIG" ]; then
  say "Creating $CONFIG"
  cp "$ROOT/config.env.example" "$CONFIG"
  if [ "$USE_DOCKER" = 1 ] && docker volume inspect knowledge-base_kb-pgdata >/dev/null 2>&1; then
    # Postgres sets the password only when the volume is first initialised. An existing volume
    # without an imported configuration was created with the defaults of config.env.example.
    warn "existing data volume found: keeping the default credentials (if the self-test fails, use --import-env)"
  elif [ "$USE_DOCKER" = 1 ]; then
    pw="$(random_hex)"
    set_env POSTGRES_PASSWORD "$pw"
    set_env DATABASE_URL "postgres://$(get_env POSTGRES_USER):${pw}@localhost:$(get_env POSTGRES_PORT)/$(get_env POSTGRES_DB)?sslmode=disable"
  fi
else
  say "Keeping the existing configuration $CONFIG"
fi
chmod 600 "$CONFIG"
[ "$USE_DOCKER" = 0 ] && [ -n "$DATABASE_URL_ARG" ] && set_env DATABASE_URL "$DATABASE_URL_ARG"
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
  "${COMPOSE[@]}" up -d --wait
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
      ollama pull "$model" || warn "could not pull $model (is the Ollama server running?)"
    fi
  else
    warn "Ollama not found (https://ollama.com/download). Afterwards: ollama pull $model"
  fi
fi

# ---------- Build and self-test ----------
say "Installing the server binary"
bin="$("$KB" --ensure)"
say "Self-test (database + embedding)"
KB_ENV_FILE="$CONFIG" "$bin" --check || die "Self-test failed (see above). After fixing it, run the setup again."

say "Done. Configuration: $CONFIG"
echo "    Run /reload-plugins (or restart Claude Code) so that the MCP server connects."
