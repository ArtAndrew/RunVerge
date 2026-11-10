#!/bin/sh
# SPDX-License-Identifier: Elastic-2.0
set -eu

POSTGRES_PASSWORD=$(cat /pilot-db/password)
RUNVERGE_APP_VERIFIER=$(cat /pilot-db/verifier)
RUNVERGE_CREDENTIAL_PROFILE=$(cat /pilot-db/profile)
export POSTGRES_PASSWORD RUNVERGE_APP_VERIFIER RUNVERGE_CREDENTIAL_PROFILE

case "$#" in
  0) exec /usr/local/bin/docker-entrypoint.sh postgres ;;
  1) [ "$1" = health ] && exec /usr/local/bin/runverge-db-healthcheck ;;
esac
exit 1
