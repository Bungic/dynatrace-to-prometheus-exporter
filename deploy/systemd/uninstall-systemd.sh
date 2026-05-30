#!/usr/bin/env bash
#
# uninstall-systemd.sh
# Removes the systemd deployment. Preserves /etc/dynatrace-exporter/env so a reinstall picks up
# the same credentials. Pass PURGE=1 to delete the env file and the service user too.

set -euo pipefail

BINARY_NAME="dynatrace-exporter"
SERVICE_USER="dynatrace-exporter"
BIN_DIR="/usr/local/bin"
ETC_DIR="/etc/${BINARY_NAME}"
UNIT_DIR="/etc/systemd/system"

[[ $EUID -eq 0 ]] || { echo "must run as root (try: sudo $0)" >&2; exit 1; }

systemctl disable --now "${BINARY_NAME}" 2>/dev/null || true
rm -f "${UNIT_DIR}/${BINARY_NAME}.service"
systemctl daemon-reload
rm -f "${BIN_DIR}/${BINARY_NAME}"

if [[ "${PURGE:-0}" == "1" ]]; then
  rm -rf "${ETC_DIR}"
  if id "$SERVICE_USER" >/dev/null 2>&1; then
    userdel "$SERVICE_USER" 2>/dev/null || true
  fi
  echo "purged: binary, unit, env dir, service user."
else
  echo "uninstalled binary + unit. preserved ${ETC_DIR}/.env and user '${SERVICE_USER}'."
  echo "to remove those too: sudo PURGE=1 $0"
fi
