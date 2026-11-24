#!/bin/bash
# Runner container entrypoint.
#
# The JIT config and the job-check token arrive on stdin (two lines) so they
# never appear in the container's env, args or labels. The token and the
# scaler endpoint go to files only the hook reads; the JIT config is handed
# to the runner, which removes it from its own environment.
set -eu

STATE_DIR=/home/runner/.scaler

jit=''
token=''
# Both stdin lines must end with \n; the scaler must send the final newline.
if ! IFS= read -r jit || [ -z "$jit" ]; then
	echo 'scaler-entrypoint: no JIT config on stdin' >&2
	exit 1
fi
if ! IFS= read -r token || [ -z "$token" ]; then
	echo 'scaler-entrypoint: no job-check token on stdin' >&2
	exit 1
fi
: "${SCALER_JOBCHECK_URL:?}" "${SCALER_RUNNER_NAME:?}" "${BUILDKIT_HOST:?}"

old_umask=$(umask)
umask 077
mkdir -p "$STATE_DIR"
chmod 0700 "$STATE_DIR"
rm -f "$STATE_DIR/token" "$STATE_DIR/endpoint"
printf '%s\n' "$token" >"$STATE_DIR/token"
printf '%s\n%s\n' "$SCALER_JOBCHECK_URL" "$SCALER_RUNNER_NAME" >"$STATE_DIR/endpoint"
chmod 0400 "$STATE_DIR/token" "$STATE_DIR/endpoint"
# Jobs must not inherit 077: files they create (checkouts, build contexts)
# would end up unreadable inside the images they build.
umask "$old_umask"

# Only records the builder; it does not wait for buildkitd to be ready.
docker buildx create --driver remote --name scaler --use "$BUILDKIT_HOST"

export ACTIONS_RUNNER_INPUT_JITCONFIG="$jit"
unset jit token
exec "$@"
