#!/usr/bin/env bash
# pg-dump.sh — dump the database of the storage core.
#
# Runs ON the node host, as the user that owns the lane:
#
#   pg-dump.sh [--lane prod|staging]
#
# The repositories of a lane live on disk, but what makes them a storage core
# lives in PostgreSQL: the changeset log, the mirror targets and their
# publications, the policies. The bucket and the repositories have a backup
# origin (see backup-origin-gate.sh); the database needs its own dump, and this
# is it.
#
# The dump is ~/bin/pg_backup.sh — dump, a check that the archive is a readable
# pg_dump TOC, a restore drill that compares the row count of every user table,
# and a rotation — pointed at the database the lane's config.toml names. It is
# a read: nothing here writes to the lane.
set -euo pipefail

ROOT="${GITAI_ROOT:-$HOME/opt/git-ai}"
PG_BACKUP="${PG_BACKUP:-$HOME/bin/pg_backup.sh}"
# The PostgreSQL server is not part of the lane: the container is shared, and
# its published port is mapped to the port inside it.
PG_CONTAINER="${PG_CONTAINER:-pigsty-postgres}"
PG_CONTAINER_PORT="${PG_CONTAINER_PORT:-25432}"
PG_HOST_PORT="${PG_HOST_PORT:-15432}"
PGUSER="${PGUSER:-postgres}"
KEEP="${KEEP:-14}"

lane=prod
while [ $# -gt 0 ]; do
  case "$1" in
    --lane) lane="${2:-}"; shift 2 ;;
    -h|--help) sed -n '2,12p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done

case "$lane" in
  prod) ;;
  staging) ROOT="${GITAI_STAGING_ROOT:-$HOME/opt/git-ai-staging}" ;;
  *) printf 'unknown lane %s: use prod or staging\n' "$lane" >&2; exit 2 ;;
esac

config="$ROOT/config.toml"
[ -r "$config" ] || { printf 'no config at %s\n' "$config" >&2; exit 1; }
[ -x "$PG_BACKUP" ] || { printf 'no backup script at %s\n' "$PG_BACKUP" >&2; exit 1; }

# database = "postgres://USER@HOST:PORT/DB?sslmode=disable"
url="$(sed -n 's/^[[:space:]]*database[[:space:]]*=[[:space:]]*"postgres:\/\/\([^"]*\)".*/\1/p' "$config" | head -1)"
[ -n "$url" ] || { printf '%s names no postgres database\n' "$config" >&2; exit 1; }

user="${url%%@*}"
user="${user%%:*}"
rest="${url#*@}"
hostport="${rest%%/*}"
port="${hostport##*:}"
db="${rest#*/}"
db="${db%%\?*}"

if [ "$port" != "$PG_HOST_PORT" ]; then
  printf 'the config names port %s, and this script assumes the shared container is published on %s; set PG_HOST_PORT\n' "$port" "$PG_HOST_PORT" >&2
  exit 1
fi

# The database is reached through the container, so the port is the one inside
# it: the published port is what the lane's config names.
exec env DB="$db" PORT="$PG_CONTAINER_PORT" PGUSER="$PGUSER" CONTAINER="$PG_CONTAINER" \
  DEST="${DEST:-$HOME/backups/pg-gitai}" KEEP="$KEEP" "$PG_BACKUP"
