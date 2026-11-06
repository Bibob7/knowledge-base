#!/usr/bin/env bash
# Sets up the backend of the knowledge-base plugin (macOS/Linux). Idempotent: running it again
# repairs and updates. Called by the /knowledge-base:setup skill; can also be run by hand.
#
#   setup.sh --status                 only report what is installed and what is missing
#   setup.sh [options]                set up: config, database, embedding model, server binary, self-test
#
# By default the knowledge base is a SQLite file (~/.local/share/knowledge-base/knowledge.db),
# no database server needed. An existing configuration keeps its database.
#
# Options:
#   --database-url URL    use this database: postgres://... (Postgres with pgvector, e.g. a shared
#                         server) or sqlite:///absolute/path.db
#   --postgres            run a local Postgres with pgvector in Docker instead of SQLite
#   --migrate-to-sqlite   copy the configured Postgres database into the SQLite file, switch the
#                         configuration over and stop the Docker container (its volume is kept)
#   --embedding openai    OpenAI embeddings instead of Ollama (expects OPENAI_API_KEY in the environment)
#   --no-ollama           do not pull the embedding model
#   --import-env PATH     take over the configuration of an earlier installation (its .env)
#   --remove-legacy       remove the integration of the pre-plugin installation (hooks in
#                         ~/.claude/settings.json, ~/.claude/rules/knowledge-base.md, user-scope MCP server)
#   --purge               DELETE all knowledge: the SQLite file, the Docker Postgres and its data
#                         volume, and the configuration
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KB="$ROOT/scripts/kb"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/knowledge-base"
CONFIG="$CONFIG_DIR/config.env"
SQLITE_DEFAULT="${XDG_DATA_HOME:-$HOME/.local/share}/knowledge-base/knowledge.db"
SETTINGS="$HOME/.claude/settings.json"
LEGACY_RULES="$HOME/.claude/rules/knowledge-base.md"
VOLUME=knowledge-base_kb-pgdata
COMPOSE=(docker compose -p knowledge-base -f "$ROOT/docker-compose.yml" --env-file "$CONFIG")

STATUS=0; NO_OLLAMA=0; REMOVE_LEGACY=0; PURGE=0; POSTGRES=0; MIGRATE=0
DATABASE_URL_ARG=""; PROVIDER="ollama"; IMPORT_ENV=""

usage() { sed -n '2,24p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --status) STATUS=1 ;;
    --database-url) DATABASE_URL_ARG="${2:?--database-url needs a value}"; shift ;;
    --postgres) POSTGRES=1 ;;
    --migrate-to-sqlite) MIGRATE=1 ;;
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
[ $((POSTGRES + MIGRATE + (${#DATABASE_URL_ARG} > 0))) -le 1 ] \
  || { echo "Use only one of --database-url, --postgres and --migrate-to-sqlite." >&2; exit 2; }

say()  { printf '==> %s\n' "$*"; }
ok()   { printf '  ok   %s\n' "$*"; }
miss() { printf '  --   %s\n' "$*"; }
warn() { printf '!!  %s\n' "$*" >&2; }
die()  { printf 'Error: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

set_env() { # set_env KEY VALUE: replace (or uncomment) or append in $CONFIG (portable, without sed -i)
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
redact() { sed -E 's#(://[^:/@]+:)[^@]*@#\1xxxxx@#' <<<"$1"; }

# db_kind URL: sqlite | docker (Postgres on this machine, run by our compose file) | external
db_kind() {
  case "$1" in
    ""|sqlite:*) echo sqlite ;;
    postgres://*@localhost:*|postgres://*@127.0.0.1:*|postgresql://*@localhost:*|postgresql://*@127.0.0.1:*) echo docker ;;
    *) echo external ;;
  esac
}
# sqlite_file URL: the file of a sqlite URL (empty URL: the default file)
sqlite_file() {
  local p="${1#sqlite:}"; p="${p#//}"
  case "$p" in "") p="$SQLITE_DEFAULT" ;; "~/"*) p="$HOME/${p#\~/}" ;; esac
  echo "$p"
}
docker_ready() { have docker && docker compose version >/dev/null 2>&1 && docker info >/dev/null 2>&1; }
require_docker() {
  have docker || die "Docker is missing (it is only needed for --postgres; the default SQLite needs nothing)."
  docker compose version >/dev/null 2>&1 || die "'docker compose' (v2) is missing."
  docker info >/dev/null 2>&1 || die "The Docker daemon is not running."
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
  local bin url kind file
  say "Status"
  if [ -f "$CONFIG" ]; then ok "configuration $CONFIG"; else miss "configuration $CONFIG (missing)"; fi
  url="$(get_env DATABASE_URL)"; kind="$(db_kind "$url")"
  case "$kind" in
    sqlite)
      file="$(sqlite_file "$url")"
      if [ -f "$file" ]; then ok "database: SQLite $file ($(du -h "$file" | cut -f1 | tr -d ' '))"
      else miss "database: SQLite $file (created by the setup)"; fi ;;
    docker)
      if docker_ready && [ "$(docker inspect -f '{{.State.Health.Status}}' knowledge-base-postgres 2>/dev/null)" = healthy ]; then
        ok "database: Postgres in Docker, container knowledge-base-postgres (healthy)"
      else miss "database: Postgres in Docker, container knowledge-base-postgres not running (or Docker not available)"; fi
      echo "       Docker is optional now: --migrate-to-sqlite moves the knowledge base into a SQLite file." ;;
    external) ok "database: Postgres $(redact "$url")" ;;
  esac
  if have ollama; then ok "Ollama"; else miss "Ollama (https://ollama.com/download; not needed with --embedding openai)"; fi
  if have go; then ok "Go $(go env GOVERSION)"; else ok "Go not installed (only needed without a release binary)"; fi
  bin="$("$KB" --path)"
  if [ -n "$bin" ] && [ -x "$bin" ]; then ok "server binary $bin"; else miss "server binary (not installed yet)"; fi
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
  url="$(get_env DATABASE_URL)"; [ "$(db_kind "$url")" = sqlite ] || url=""
  file="$(sqlite_file "$url")"
  for f in "$file" "$file-wal" "$file-shm"; do
    if [ -f "$f" ]; then say "Deleting $f"; rm -f "$f"; fi
  done
  if docker_ready && docker volume inspect "$VOLUME" >/dev/null 2>&1; then
    say "Stopping Postgres and deleting the data volume $VOLUME"
    if [ -f "$CONFIG" ]; then "${COMPOSE[@]}" down -v; else docker compose -p knowledge-base -f "$ROOT/docker-compose.yml" down -v; fi
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

# ---------- Configuration ----------
case "$PROVIDER" in
  ollama) ;;
  openai) [ -n "${OPENAI_API_KEY:-}" ] || die "--embedding openai needs OPENAI_API_KEY in the environment." ;;
  *) die "Unknown embedding provider: $PROVIDER (ollama|openai)" ;;
esac
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
else
  say "Keeping the existing configuration $CONFIG"
fi
chmod 600 "$CONFIG"
if [ "$PROVIDER" = openai ]; then
  set_env EMBEDDING_BASE_URL "https://api.openai.com/v1"
  set_env EMBEDDING_MODEL "text-embedding-3-small"
  set_env EMBEDDING_DIM "1536"
  set_env EMBEDDING_DOC_PREFIX ""
  set_env EMBEDDING_QUERY_PREFIX ""
  set_env EMBEDDING_API_KEY "$OPENAI_API_KEY"
  NO_OLLAMA=1
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

say "Installing the server binary"
bin="$("$KB" --ensure)"

# ---------- Database ----------
CURRENT_URL="$(get_env DATABASE_URL)"
if [ "$MIGRATE" = 1 ]; then
  [ "$(db_kind "$CURRENT_URL")" != sqlite ] || die "The knowledge base already uses SQLite ($(sqlite_file "$CURRENT_URL"))."
  if [ "$(db_kind "$CURRENT_URL")" = docker ]; then
    require_docker
    say "Starting Postgres (Docker) to read from it"
    "${COMPOSE[@]}" up -d --wait
  fi
  say "Copying the knowledge base into $SQLITE_DEFAULT"
  KB_ENV_FILE="$CONFIG" "$bin" migrate --to "sqlite://$SQLITE_DEFAULT" || die "Migration failed (see above); nothing was switched over."
  # Keep the old URL as a comment right above the new one: switching back is swapping them.
  tmp="$(mktemp)"
  awk -v d="$(date +%Y-%m-%d)" -v new="DATABASE_URL=sqlite://$SQLITE_DEFAULT" '
    /^DATABASE_URL=/ {print "# Before the migration to SQLite on " d ": " $0; print new; next} {print}' "$CONFIG" >"$tmp"
  cat "$tmp" >"$CONFIG"; rm -f "$tmp"
  if [ "$(db_kind "$CURRENT_URL")" = docker ]; then
    say "Stopping the Postgres container (the data volume $VOLUME is kept as a backup)"
    "${COMPOSE[@]}" stop
  fi
elif [ -n "$DATABASE_URL_ARG" ]; then
  case "$DATABASE_URL_ARG" in
    sqlite:*|postgres://*|postgresql://*) ;;
    *) die "--database-url must start with postgres:// or sqlite://" ;;
  esac
  say "Using the database $(redact "$DATABASE_URL_ARG")"
  set_env DATABASE_URL "$DATABASE_URL_ARG"
elif [ "$POSTGRES" = 1 ] || [ "$(db_kind "$CURRENT_URL")" = docker ]; then
  require_docker
  if [ -z "$(get_env POSTGRES_PASSWORD)" ]; then
    set_env POSTGRES_USER kb; set_env POSTGRES_DB knowledge_base; set_env POSTGRES_PORT 5433
    if docker volume inspect "$VOLUME" >/dev/null 2>&1; then
      # Postgres sets the password only when the volume is first initialised; an existing
      # volume without an imported configuration was created with the defaults.
      warn "existing data volume found: keeping the default credentials (if the self-test fails, use --import-env)"
      set_env POSTGRES_PASSWORD kb
    else
      set_env POSTGRES_PASSWORD "$(random_hex)"
    fi
  fi
  [ "$POSTGRES" = 1 ] && set_env DATABASE_URL "postgres://$(get_env POSTGRES_USER):$(get_env POSTGRES_PASSWORD)@localhost:$(get_env POSTGRES_PORT)/$(get_env POSTGRES_DB)?sslmode=disable"
  say "Starting Postgres + pgvector (Docker)"
  "${COMPOSE[@]}" up -d --wait
elif [ "$(db_kind "$CURRENT_URL")" = sqlite ]; then
  say "Database: SQLite $(sqlite_file "$CURRENT_URL") (no server needed)"
else
  say "Using the configured database $(redact "$CURRENT_URL")"
fi

say "Self-test (database + embedding)"
KB_ENV_FILE="$CONFIG" "$bin" --check || die "Self-test failed (see above). After fixing it, run the setup again."

say "Done. Configuration: $CONFIG"
echo "    Run /reload-plugins (or restart Claude Code) so that the MCP server connects."
if [ "$MIGRATE" = 1 ]; then
  echo "    Run it right away: until then, running sessions still write into Postgres."
  [ "$(db_kind "$CURRENT_URL")" = docker ] \
    && echo "    When everything works, delete the old data: docker volume rm $VOLUME"
fi
