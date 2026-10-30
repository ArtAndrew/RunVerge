#!/bin/sh
# SPDX-License-Identifier: Elastic-2.0
# Entrypoint runs this only for a newly initialized cluster, after role creation.
set -eu
case "${RUNVERGE_CREDENTIAL_PROFILE:-}" in
  ''|*[!0123456789abcdef]*) exit 1 ;;
esac
[ "${#RUNVERGE_CREDENTIAL_PROFILE}" -eq 32 ] || exit 1
umask 077
set -C
printf 'runverge-app-owner-v1:%s\n' "$RUNVERGE_CREDENTIAL_PROFILE" > "$PGDATA/runverge-credential-profile"
