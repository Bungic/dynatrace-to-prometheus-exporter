#!/usr/bin/env bash
#
# install-systemd.sh
# One-shot installer for the systemd deployment of dynatrace-to-prometheus-exporter.
#
# Usage:
#   sudo ./deploy/install-systemd.sh                       # builds binary from source
#   sudo BINARY=/path/to/dynatrace-exporter ./deploy/install-systemd.sh   # uses prebuilt binary
#   sudo VERSION=1.0.0 ./deploy/install-systemd.sh         # bakes version tag into the binary
#
# Idempotent: existing env file is preserved, existing user is reused, daemon-reload runs every time.

set -euo pipefail

BINARY_NAME="dynatrace-exporter"
SERVICE_USER="dynatrace-exporter"
BIN_DIR="/usr/local/bin"
ETC_DIR="/etc/${BINARY_NAME}"
UNIT_DIR="/etc/systemd/system"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

log()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m %s\n' "$*" >&2; }
fail() { printf '\033[1;31mxx\033[0m %s\n' "$*" >&2; exit 1; }

require_root() {
  [[ $EUID -eq 0 ]] || fail "must run as root (try: sudo $0)"
}

resolve_binary() {
  if [[ -n "${BINARY:-}" ]]; then
    [[ -x "$BINARY" ]] || fail "BINARY points to non-executable file: $BINARY"
    log "using prebuilt binary: $BINARY"
    return
  fi
  command -v go >/dev/null || fail "go is not installed and BINARY env was not set"
  local v="${VERSION:-dev}"
  log "building binary from ${REPO_ROOT} (version=${v})"
  (cd "$REPO_ROOT" && go build -trimpath -ldflags="-s -w -X main.version=${v}" -o "/tmp/${BINARY_NAME}" .)
  BINARY="/tmp/${BINARY_NAME}"
}

install_binary() {
  install -m 0755 "$BINARY" "${BIN_DIR}/${BINARY_NAME}"
  log "installed: ${BIN_DIR}/${BINARY_NAME}"
}

ensure_user() {
  if id "$SERVICE_USER" >/dev/null 2>&1; then
    log "user already exists: $SERVICE_USER"
  else
    useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
    log "created user: $SERVICE_USER"
  fi
}

install_env_file() {
  mkdir -p "$ETC_DIR"
  if [[ -f "${ETC_DIR}/.env" ]]; then
    log "env file already exists, leaving it alone: ${ETC_DIR}/.env"
  else
    install -m 0640 "${SCRIPT_DIR}/dynatrace-exporter.env" "${ETC_DIR}/.env"
    log "installed env template: ${ETC_DIR}/.env"
  fi
  chown root:"$SERVICE_USER" "${ETC_DIR}/.env"
  chmod 0640 "${ETC_DIR}/.env"
}

install_unit() {
  install -m 0644 "${SCRIPT_DIR}/dynatrace-exporter.service" "${UNIT_DIR}/${BINARY_NAME}.service"
  systemctl daemon-reload
  log "installed unit: ${UNIT_DIR}/${BINARY_NAME}.service"
}

start_or_warn() {
  if grep -qE 'REPLACE_ME|YOUR_ENV' "${ETC_DIR}/.env"; then
    warn "env file still has placeholder values (REPLACE_ME / YOUR_ENV)"
    warn "edit ${ETC_DIR}/.env, then run:"
    warn "  sudo systemctl enable --now ${BINARY_NAME}"
    return
  fi
  systemctl enable --now "${BINARY_NAME}"
  log "service enabled and started"
  echo
  systemctl status "${BINARY_NAME}" --no-pager || true
  echo
  log "tail logs with: journalctl -u ${BINARY_NAME} -f"
}

main() {
  require_root
  resolve_binary
  install_binary
  ensure_user
  install_env_file
  install_unit
  start_or_warn
}

main "$@"
