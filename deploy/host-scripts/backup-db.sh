#!/usr/bin/env bash
# backup-db.sh — dump a lane's database before a release touches it.
#
# Runs ON the deploy host:
#
#   LANE_PG_DB VERSION BACKUP_DIR PG_CONTAINER PG_IN_CONTAINER_PORT
#   KEEP (optional, default 10)
#
# A promotion installs a release whose migrations run against the live
# database, and the NAS's daily backup script does not dump the storage core's
# database at all. The dump is taken through the PostgreSQL container, which
# needs no credentials of ours, and it is verified by gzip before its path is
# printed as the last line, so the promotion can cite it.
set -euo pipefail

db="${LANE_PG_DB:?}"
version="${VERSION:?}"
backup_dir="${BACKUP_DIR:?}"
keep="${KEEP:-10}"

mkdir -p "$backup_dir"
chmod 700 "$backup_dir"
out="$backup_dir/$db-$version-$(date +%Y%m%d-%H%M%S).sql.gz"

docker exec -u postgres "$PG_CONTAINER" \
  pg_dump -p "$PG_IN_CONTAINER_PORT" -d "$db" | gzip >"$out.part"
gzip -t "$out.part" || { echo "the dump of $db is not valid gzip" >&2; exit 1; }
mv "$out.part" "$out"
chmod 600 "$out"

# Keep the newest dumps of this database; the rest are pruned by age order.
ls -1t "$backup_dir/$db-$version"-*.sql.gz 2>/dev/null | tail -n "+$((keep + 1))" |
  while read -r old; do rm -f "$old"; done

printf '  database %s dumped to %s (%s)\n' "$db" "$out" "$(du -h "$out" | cut -f1)"
printf '%s\n' "$out"
