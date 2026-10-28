#!/usr/bin/env bash
set -euo pipefail

IFS= read -r -s -p "New password for ${HFPROXY_ADMIN_USERNAME:-admin}: " HFPROXY_LOCAL_ADMIN_PASSWORD
printf '\n'
trap 'unset HFPROXY_LOCAL_ADMIN_PASSWORD' EXIT

printf '%s' "$HFPROXY_LOCAL_ADMIN_PASSWORD" |
  docker compose exec -T hfproxy hfproxy admin reset-password \
    --username "${HFPROXY_ADMIN_USERNAME:-admin}" \
    --password-file -
