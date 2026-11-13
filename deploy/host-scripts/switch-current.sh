#!/usr/bin/env bash
# switch-current.sh — point a lane at one of its installed releases and restart
# the lane's processes.
#
# Runs ON the deploy host:
#
#   LANE_DIR LANE_CONTAINER LANE_PROGRAMS VERSION
#
# The switch is a symlink swap: `current` names a directory under releases/ and
# `bin` follows `current/bin`. Nothing is deleted, so the previous release stays
# installed and a rollback is another switch.
set -euo pipefail

target="$LANE_DIR/releases/$VERSION"
[ -x "$target/bin/gitaly" ] || { echo "no gitaly in $target" >&2; exit 1; }

# A lane that was installed by hand keeps the release that is running today:
# copy it into releases/ before bin starts following current, so that the
# release that ran before the first switch is still installed.
if [ ! -L "$LANE_DIR/bin" ] && [ -d "$LANE_DIR/bin" ]; then
  legacy="$LANE_DIR/releases/legacy-$(date +%Y%m%d-%H%M%S)"
  mkdir -p "$legacy"
  cp -a "$LANE_DIR/bin" "$legacy/bin"
  [ -f "$LANE_DIR/config.toml" ] && cp -a "$LANE_DIR/config.toml" "$legacy/config.toml"
  echo "  kept the unversioned bin/ as $(basename "$legacy")"
fi

# Atomic swap: a new symlink is moved over the old one.
ln -sfn "$target" "$LANE_DIR/.current.new"
mv -Tf "$LANE_DIR/.current.new" "$LANE_DIR/current"
echo "  current -> $VERSION"

if [ ! -L "$LANE_DIR/bin" ]; then
  if [ -e "$LANE_DIR/bin" ]; then
    mv "$LANE_DIR/bin" "$LANE_DIR/bin.unversioned-$(date +%Y%m%d-%H%M%S)"
  fi
  ln -sfn "current/bin" "$LANE_DIR/bin"
  echo "  bin -> current/bin"
fi

# The runtime directory of the previous process: inside a container every start
# has pid 1, so the directory of a killed process is still there under the same
# name. Releases before this one refused to start over it; the cleanup keeps a
# rollback to such a release working.
rm -rf "$LANE_DIR"/run/gitaly-[0-9]* 2>/dev/null || true

# The server: a fresh container, so the mount and the command are the lane's
# and not those of a container that was built months ago.
docker rm -f "$LANE_CONTAINER" >/dev/null 2>&1 || true
docker run -d --name "$LANE_CONTAINER" --restart unless-stopped \
  --network host --user 1002:1003 \
  -v "$LANE_DIR:$LANE_DIR" -w "$LANE_DIR" \
  ubuntu:24.04 "$LANE_DIR/run-gitaly.sh" >/dev/null
echo "  container $LANE_CONTAINER started"

# The cache tiers run the release's binary too.
supervisorctl -c "$HOME/.supervisord/supervisord.conf" reread >/dev/null
supervisorctl -c "$HOME/.supervisord/supervisord.conf" update >/dev/null
for prog in $LANE_PROGRAMS; do
  supervisorctl -c "$HOME/.supervisord/supervisord.conf" restart "$prog" >/dev/null
done
echo "  cache tiers restarted ($LANE_PROGRAMS)"
