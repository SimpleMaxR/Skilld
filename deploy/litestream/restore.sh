#!/usr/bin/env bash
set -euo pipefail

# One-click SQLite Disaster Recovery Script for SkillVault using Litestream (Cloudflare R2 / S3 / File)

CONFIG_PATH="/etc/skillvault/litestream.yml"
DB_PATH="/var/lib/skillvault/skillvault.db"
SERVICE_NAME="skillvault-server"
LITESTREAM_SERVICE="litestream"
ENV_FILE="/etc/skillvault/litestream.env"
TIMESTAMP=""
SKIP_SERVICE=0
DRY_RUN=0

usage() {
  cat <<'EOF'
SkillVault One-Click Litestream Disaster Recovery

Usage:
  restore.sh [options]

Options:
  --config <path>       Path to litestream.yml (default: /etc/skillvault/litestream.yml)
  --db-path <path>      Target SQLite DB path (default: /var/lib/skillvault/skillvault.db)
  --env-file <path>     Optional env file containing R2 credentials (default: /etc/skillvault/litestream.env)
  --timestamp <rfc3339> Optional point-in-time restore timestamp (e.g. 2026-11-24T12:00:00Z)
  --service <name>      Systemd service name to stop/start (default: skillvault-server)
  --skip-service        Skip stopping/starting systemd services (for standalone/non-systemd restore)
  --dry-run             Print recovery steps without modifying files or services
  -h, --help            Show this help text
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --config)
      CONFIG_PATH="${2:?missing value for --config}"
      shift 2
      ;;
    --db-path)
      DB_PATH="${2:?missing value for --db-path}"
      shift 2
      ;;
    --env-file)
      ENV_FILE="${2:?missing value for --env-file}"
      shift 2
      ;;
    --timestamp)
      TIMESTAMP="${2:?missing value for --timestamp}"
      shift 2
      ;;
    --service)
      SERVICE_NAME="${2:?missing value for --service}"
      shift 2
      ;;
    --skip-service)
      SKIP_SERVICE=1
      shift
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Error: unknown option '$1'" >&2
      usage >&2
      exit 1
      ;;
  esac
done

log() {
  printf '[restore %s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"
}

run_cmd() {
  if [[ "$DRY_RUN" -eq 1 ]]; then
    log "[dry-run] $*"
  else
    "$@"
  fi
}

if [[ "$DRY_RUN" -eq 0 ]] && ! command -v litestream >/dev/null 2>&1; then
  echo "Error: 'litestream' binary not found in PATH." >&2
  exit 1
fi

if [[ ! -f "$CONFIG_PATH" ]]; then
  echo "Error: Litestream config not found at '$CONFIG_PATH'." >&2
  exit 1
fi

if [[ -f "$ENV_FILE" ]]; then
  log "Loading credentials from $ENV_FILE"
  set -a
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  set +a
fi

RESTORE_ARGS=(-config "$CONFIG_PATH" -o "${DB_PATH}.restoring.$$")
if [[ -n "$TIMESTAMP" ]]; then
  RESTORE_ARGS+=(-timestamp "$TIMESTAMP")
fi
RESTORE_ARGS+=("$DB_PATH")

STOPPED_SERVER=0
STOPPED_LITESTREAM=0

cleanup_on_error() {
  rm -f "${DB_PATH}.restoring.$$" "${DB_PATH}.restoring.$$-wal" "${DB_PATH}.restoring.$$-shm" || true
}
trap cleanup_on_error EXIT

if [[ "$SKIP_SERVICE" -eq 0 ]] && command -v systemctl >/dev/null 2>&1; then
  if systemctl is-active --quiet "$LITESTREAM_SERVICE"; then
    log "Stopping systemd service: $LITESTREAM_SERVICE"
    run_cmd systemctl stop "$LITESTREAM_SERVICE"
    STOPPED_LITESTREAM=1
  fi
  if systemctl is-active --quiet "$SERVICE_NAME"; then
    log "Gracefully stopping systemd service (draining 8192 queue): $SERVICE_NAME"
    run_cmd systemctl stop "$SERVICE_NAME"
    STOPPED_SERVER=1
  fi
fi

DB_DIR="$(dirname "$DB_PATH")"
run_cmd mkdir -p "$DB_DIR"

STAGING_DB="${DB_PATH}.restoring.$$"
log "Restoring SQLite snapshot + WAL stream from Litestream replica into $STAGING_DB"
run_cmd litestream restore "${RESTORE_ARGS[@]}"

if [[ "$DRY_RUN" -eq 0 ]]; then
  if [[ ! -s "$STAGING_DB" ]]; then
    echo "Error: restored database file '$STAGING_DB' is missing or empty." >&2
    exit 1
  fi

  if command -v sqlite3 >/dev/null 2>&1; then
    log "Running PRAGMA integrity_check on restored database"
    CHECK_RES="$(sqlite3 "$STAGING_DB" 'PRAGMA integrity_check;' | tr -d '\r\n')"
    if [[ "$CHECK_RES" != "ok" ]]; then
      echo "Error: SQLite integrity_check failed: '$CHECK_RES'" >&2
      exit 1
    fi
    log "Integrity check passed: ok"
  fi

  BACKUP_STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
  BACKUP_DIR="${DB_PATH}.pre-restore.${BACKUP_STAMP}.bak"
  if [[ -f "$DB_PATH" || -f "${DB_PATH}-wal" || -f "${DB_PATH}-shm" ]]; then
    log "Backing up existing local database files to $BACKUP_DIR"
    mkdir -p "$BACKUP_DIR"
    [[ -f "$DB_PATH" ]] && mv "$DB_PATH" "$BACKUP_DIR/"
    [[ -f "${DB_PATH}-wal" ]] && mv "${DB_PATH}-wal" "$BACKUP_DIR/"
    [[ -f "${DB_PATH}-shm" ]] && mv "${DB_PATH}-shm" "$BACKUP_DIR/"
  fi

  mv "$STAGING_DB" "$DB_PATH"
  chmod 0640 "$DB_PATH"
  if id skillvault >/dev/null 2>&1; then
    chown skillvault:skillvault "$DB_PATH" || true
  fi
  log "Restored database atomically installed at $DB_PATH"
fi

if [[ "$SKIP_SERVICE" -eq 0 ]] && command -v systemctl >/dev/null 2>&1; then
  if [[ "$STOPPED_SERVER" -eq 1 ]]; then
    log "Starting systemd service: $SERVICE_NAME"
    run_cmd systemctl start "$SERVICE_NAME"
  fi
  if [[ "$STOPPED_LITESTREAM" -eq 1 ]]; then
    log "Starting systemd service: $LITESTREAM_SERVICE"
    run_cmd systemctl start "$LITESTREAM_SERVICE"
  fi
fi

log "Disaster recovery completed successfully."
