#!/bin/bash
# Runner container entrypoint.
#
# The JIT config and the job-check token arrive on stdin (two lines) so they
# never appear in the container's env, args or labels. The token and the
# scaler endpoint go to files only the hook reads; the JIT config is handed
# to the runner. The runner clears it only from its own environment block and
# writes the decoded credentials to files in its root folder, so job steps
# running as the same user may still read them (SPEC Areas of concern 18).
set -eu

STATE_DIR=/home/runner/.scaler
# Seconds to wait for buildkitd to accept connections (see wait_for_buildkit).
BUILDKIT_WAIT_SECONDS=30

# wait_for_buildkit retries a TCP connect to buildkitd's address, the
# BUILDKIT_HOST value, once a second for up to BUILDKIT_WAIT_SECONDS, so a
# job's first build does not meet a buildkitd that is still starting. The
# BuildKit container is created before the runner, but its daemon may not
# listen yet. If it never answers, the entrypoint goes on as it always did
# and the build fails at its own step. The probe is bash's /dev/tcp. Each
# probe is bounded to 2 seconds by coreutils timeout (in the Ubuntu base
# image) so a connect to an address that drops packets cannot outlast the
# wait; without timeout the probe is unbounded, and the elapsed-time check
# below still ends the wait at the first failed probe after the limit.
probe_tcp() {
	if command -v timeout >/dev/null 2>&1; then
		timeout 2 bash -c 'exec 3<>"/dev/tcp/$0/$1"' "$1" "$2" 2>/dev/null
	else
		(exec 3<>"/dev/tcp/$1/$2") 2>/dev/null
	fi
}

wait_for_buildkit() {
	case $BUILDKIT_HOST in
	tcp://*) ;;
	*) return 0 ;;
	esac
	local hostport=${BUILDKIT_HOST#tcp://} start=$SECONDS
	local host=${hostport%:*} port=${hostport##*:}
	while ! probe_tcp "$host" "$port"; do
		if [ $((SECONDS - start)) -ge "$BUILDKIT_WAIT_SECONDS" ]; then
			echo "scaler-entrypoint: buildkitd at $BUILDKIT_HOST did not answer within ${BUILDKIT_WAIT_SECONDS}s; continuing" >&2
			return 0
		fi
		sleep 1
	done
}

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

# The builder is only recorded here; buildkitd is waited for first so it is
# ready when the job builds.
wait_for_buildkit
docker buildx create --driver remote --name scaler --use "$BUILDKIT_HOST"

export ACTIONS_RUNNER_INPUT_JITCONFIG="$jit"
unset jit token
exec "$@"
