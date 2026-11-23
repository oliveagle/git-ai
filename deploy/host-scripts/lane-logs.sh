#!/usr/bin/env bash
# lane-logs.sh — why the lane's container is not healthy.
#
# Runs ON the deploy host. The health check says what does not answer (a port
# with no listener, a storage core that does not reply); this says why. A
# release that refuses its configuration exits before it binds a port, and the
# only evidence is the container's own last lines — which the rollback removes
# together with the container. So the deploy dumps them before it rolls back.
#
#   LANE_CONTAINER (required)
set -uo pipefail

: "${LANE_CONTAINER:?}"
printf 'container: %s\n' "$LANE_CONTAINER"
docker inspect "$LANE_CONTAINER" \
  --format 'state={{.State.Status}} exit={{.State.ExitCode}} restarts={{.RestartCount}} error={{.State.Error}}' \
  2>&1 || true
printf -- '--- last 80 lines of the container log ---\n'
docker logs --tail 80 "$LANE_CONTAINER" 2>&1 || true
