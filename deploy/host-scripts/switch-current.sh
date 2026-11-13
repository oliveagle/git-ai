#!/usr/bin/env bash
# switch-current.sh — point a lane at one of its installed releases and
# restart the lane's processes.
#
# Runs ON the deploy host:
#
#   LANE_DIR LANE_CONTAINER LANE_PROGRAMS VERSION
#
# The switch is a symlink swap: `current` names a directory under
# releases/, and `bin` follows `current/bin`. Nothing is deleted, so the
# previous release stays installed and a rollback is another switch.
set -euo pipefail

target="$LANE_DIR/releases/$VERSION"
[ -x "$target/bin/gitaly" ] || { echo "no gitaly in $target" >&2; exit 1; }

if [ ! -L "$LANE_DIR/bin" ]; then
  # First deploy of this lane: the unversioned bin/ is the release that is
  # running today. Copy it into releases/ so it stays available for a
  # rollback, then let bin follow current.
  legacy="$LANE_DIR/releases/legacy-$(date +%Y%m%d-%H%M%S)"
  mkdir -p "$legacy"
  cp -a "$LANE_DIR/bin" "$legacy/bin"
  [ -f "$LANE_DIR/config.toml" ] && cp -a "$LANE_DIR/config.toml" "$legacy/config.toml"
  ln -sfn "$legacy" "$LANE_DIR/current"
  mv "$LANE_DIR/bin" "$LANE_DIR/bin.unversioned-$(date +%Y%m%d-%H%M%S)"
  ln -sfn "current/bin" "$LANE_DIR/bin"
  echo "  migrated the unversioned bin/ into $(basename "$legacy")"
fi

# Atomic swap: a new symlink is moved over the old one.
ln -sfn "$target" "$LANE_DIR/.current.new"
mv -Tf "$LANE_DIR/.current.new" "$LANE_DIR/current"
echo "  current -> $VERSION"

# The server: a fresh container so the mount and the command follow the
# lane, not the container that was built months ago.
docker rm -f "$LANE_CONTAINER" >/dev/null 2>&1 || true
docker run -d --name "$LANE_CONTAINER" --restart unless-stopped \
  --network host --user 1002:1003 \
  -v "$LANE_DIR:$LANE_DIR" -w "$LANE_DIR" \
  ubuntu:24.04 "$LANE_DIR/run-gitaly.sh" >/dev/null
echo "  container $LANE_CONTAINER started"

# The cache tiers run the binary of the release too.
supervisorctl -c "$HOME/.supervisord/supervisord.conf" reread >/dev/null
supervisorctl -c "$HOME/.supervisord/supervisord.conf" update >/dev/null
for prog in $LANE_PROGRAMS; do
  supervisorctl -c "$HOME/.supervisord/supervisord.conf" restart "$prog" >/dev/null
done
echo "  cache tiers restarted ($LANE_PROGRAMS)"
