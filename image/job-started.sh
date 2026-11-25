#!/bin/bash
PATH=/usr/bin:/bin
# Job-started hook. The runner runs it as `bash -e <path>` before any step.
#
# A hook that ends without success still lets always() and failure() steps
# run, so this hook never ends on its own except on an allow verdict. On any
# other end (signal, unexpected error) the trap SIGKILLs the runner processes
# first so the container exits before another step can start.
#
# The one exception is a deny verdict: the hook sends SIGINT to the
# Runner.Listener among its ancestors and waits. The runner then shuts down:
# GitHub marks the job failed, the Worker cancels this hook with a signal,
# and every remaining step, if: always() included, is skipped on runner
# shutdown (actions/runner v2.338.2 StepsRunner.cs, "Skip evaluate condition
# on runner shutdown"). That signal ends the hook with 1 and no kill, so the
# Listener finishes its shutdown and the container exits 0. This is the only
# end other than allow without the kill, and only after the hook itself
# asked the Listener to stop because the scaler said deny. When no Listener
# is found or the signal cannot be sent, the hook kills the runner processes
# as on any other end. The scaler removes the container if it has not
# stopped 30 seconds after the deny.
#
# The scaler address, runner name and token come only from files written by
# the entrypoint, and every command is called by absolute path, so
# environment variables a job can influence cannot redirect the query.
# Compatible with bash 3.2 so the tests also run on macOS.

STATE_DIR=/home/runner/.scaler
PROC_DIR=/proc
START_PID=$PPID
# Upper bound in seconds for one curl call. The runner sends SIGINT, waits
# 7.5s, then SIGTERM; staying below that keeps the trap responsive.
CHILD_MAX=5
CONNECT_TIMEOUT=2
POLL_INTERVAL=2
RECORD=0

token_file=$STATE_DIR/token
endpoint_file=$STATE_DIR/endpoint
body_file=$STATE_DIR/verdict.body
code_file=$STATE_DIR/verdict.code

allowed=0
# stopping is set just before the hook asks the Listener to stop on a deny;
# released marks the end that follows it, which must not kill.
stopping=0
released=0
killing=0
killed=' '
ancestors=' '

kill_one() {
	case $1 in '' | *[!0-9]*) return 0 ;; esac
	if [ "$1" -le 1 ] || [ "$1" -eq "$$" ]; then
		return 0
	fi
	case $killed in *" $1 "*) return 0 ;; esac
	killed="$killed$1 "
	# Kill before logging: a full stderr pipe must not hold up the kill.
	kill -KILL "$1" 2>/dev/null || true
	printf 'scaler-job-started: kill -KILL %s\n' "$1" >&2 || true
}

# walk_chain sets chain to the ancestors of START_PID, nearest first
# (Runner.Listener, run-helper.sh, run.sh). It stops at an unreadable stat,
# a ppid of 0, 1 or this hook, and after 16 steps. Builtins only.
walk_chain() {
	chain=' '
	pid=$START_PID
	depth=0
	while [ "$depth" -lt 16 ]; do
		depth=$((depth + 1))
		line=''
		if ! IFS= read -r line 2>/dev/null <"$PROC_DIR/$pid/stat"; then
			[ -n "$line" ] || break
		fi
		# comm (field 2) may contain spaces and ')', so parse after the last ')'.
		rest=${line##*\)}
		[ "$rest" != "$line" ] || break
		rest=${rest# }
		rest=${rest#* }
		ppid=${rest%% *}
		case $ppid in '' | *[!0-9]*) break ;; esac
		if [ "$ppid" -le 1 ] || [ "$ppid" -eq "$$" ] || [ "$ppid" -eq "$pid" ]; then
			break
		fi
		chain="$chain$ppid "
		pid=$ppid
	done
}

kill_runner() {
	# Ignore further signals before taking the flag, so a second signal can
	# never find the flag set while this handler has not killed anything.
	# The second group ends bash by default; ignoring it keeps one from
	# cutting the kills short. Names are valid in bash 3.2 and 5.2.
	trap '' INT TERM HUP QUIT USR1 USR2 ALRM VTALRM PROF XCPU XFSZ SYS TRAP ABRT BUS FPE ILL SEGV
	if [ "$killing" = 1 ]; then
		return 0
	fi
	killing=1

	# The parent (Runner.Worker) goes first, before anything that can fail.
	kill_one "$START_PID"

	# Ancestors read at start: once the Worker is killed and reaped its stat
	# is gone and a fresh walk from it stops at once. Killing them ends the
	# container even if the names below do not match.
	for pid in $ancestors; do
		kill_one "$pid"
	done
	walk_chain || true
	for pid in $chain; do
		kill_one "$pid"
	done

	for dir in "$PROC_DIR"/[0-9]*; do
		argv0=''
		IFS= read -r -d '' argv0 2>/dev/null <"$dir/cmdline" || true
		case $argv0 in
		*Runner.Listener | *Runner.Worker) kill_one "${dir##*/}" ;;
		esac
	done
}

on_exit() {
	if [ "$allowed" != 1 ] && [ "$released" != 1 ]; then
		kill_runner
	fi
}

on_signal() {
	if [ "$stopping" = 1 ]; then
		# The runner is shutting down at the hook's request: the Worker
		# cancels the hook, and the steps after it are skipped.
		released=1
		exit 1
	fi
	kill_runner
	exit 1
}

# find_listener sets listener to the nearest ancestor read at start whose
# argv[0] ends in Runner.Listener, or to nothing. Builtins only.
find_listener() {
	listener=''
	for pid in $ancestors; do
		case $pid in '' | *[!0-9]*) continue ;; esac
		if [ "$pid" -le 1 ] || [ "$pid" -eq "$$" ]; then
			continue
		fi
		argv0=''
		IFS= read -r -d '' argv0 2>/dev/null <"$PROC_DIR/$pid/cmdline" || true
		case $argv0 in
		*Runner.Listener)
			listener=$pid
			return 0
			;;
		esac
	done
}

# stop_runner handles a deny verdict: it asks the Listener to stop with
# SIGINT and then waits, without querying again, for the signal that
# cancels the hook. Without a Listener, or when the signal fails, it kills
# the runner processes and ends the hook.
stop_runner() {
	find_listener || true
	if [ -n "$listener" ]; then
		# stopping is set before the kill: the Worker signals the hook
		# moments after the Listener's SIGINT. The trapped signals are
		# ignored meanwhile, so none can run on_signal after the flag is
		# set but before the SIGINT is sent; one lost here is followed by
		# the runner's next (SIGTERM 7.5s after SIGINT).
		trap '' INT TERM HUP QUIT USR1 USR2 ALRM
		stopping=1
		if kill -INT "$listener" 2>/dev/null; then
			trap on_signal INT TERM HUP QUIT USR1 USR2 ALRM
			printf 'scaler-job-started: job denied; asked the runner to stop\n' || true
			# In the background so a signal reaches the trap at once.
			while :; do
				/bin/sleep "$POLL_INTERVAL" &
				wait "$!" || true
			done
		fi
		stopping=0
		trap on_signal INT TERM HUP QUIT USR1 USR2 ALRM
	fi
	printf 'scaler-job-started: job denied; cannot ask the runner to stop, killing it\n' || true
	kill_runner
	exit 1
}

# The Worker owns our stdout/stderr pipes; once it is killed a write must
# fail with EPIPE instead of killing the hook in the middle of the trap.
trap '' PIPE
trap on_exit EXIT
# Every signal the runner or a user is likely to send is trapped explicitly,
# so the kill does not depend on how a bash version runs the EXIT trap on a
# fatal signal.
trap on_signal INT TERM HUP QUIT USR1 USR2 ALRM

# Read the ancestors once now, while the Worker is alive.
walk_chain || true
ancestors=$chain

print_cmdline() {
	while IFS= read -r -d '' arg; do
		printf ' %s' "$arg"
	done 2>/dev/null <"$1" || true
	printf '\n'
}

# Diagnostic variant (build.sh --record): how the runner started the hook,
# which variable names it passed, and the ignored and blocked signal masks
# (INT and TERM must be in neither; bash cannot trap a signal ignored at
# start, and PIPE shows as ignored by this hook). Values are never written.
record() {
	{
		printf 'parent cmdline:'
		print_cmdline "$PROC_DIR/$PPID/cmdline"
		printf 'self cmdline:'
		print_cmdline "$PROC_DIR/$$/cmdline"
		printf 'env names:\n'
		while IFS= read -r -d '' kv; do
			printf '%s\n' "${kv%%=*}"
		done 2>/dev/null <"$PROC_DIR/$$/environ" || true
		printf 'signal masks:\n'
		while IFS= read -r line; do
			case $line in SigIgn:* | SigBlk:*) printf '%s\n' "$line" ;; esac
		done 2>/dev/null <"$PROC_DIR/$$/status" || true
	} 2>/dev/null >"$STATE_DIR/hook-record"
}

if [ "$RECORD" = 1 ]; then
	record || true
fi

last=''
while :; do
	url=''
	name=''
	token=''
	code=''
	body=''
	verdict=''

	{ IFS= read -r url && IFS= read -r name; } 2>/dev/null <"$endpoint_file" || true
	IFS= read -r token 2>/dev/null <"$token_file" || true
	url=${url%/}

	if [ -z "$url" ] || [ -z "$name" ] || [ -z "$token" ]; then
		state='endpoint or token file unreadable'
	elif ! : 2>/dev/null >"$body_file"; then
		# A stale body must never be read as this query's answer.
		state='cannot reset response file'
	else
		# Headers go through stdin so the token is not in any process args.
		# env -i keeps resolver and loader variables (HOSTALIASES,
		# RES_OPTIONS, LD_PRELOAD, ...) out of curl. -s without -S: failures
		# show as status 000 in the de-duplicated line below instead of one
		# error per poll. curl and sleep run in the background so a signal
		# reaches the trap at once instead of after the child ends.
		printf 'Authorization: Bearer %s\nX-Runner-Name: %s\n' "$token" "$name" 2>/dev/null |
			/usr/bin/env -i /usr/bin/curl -q --noproxy '*' -s \
				--connect-timeout "$CONNECT_TIMEOUT" --max-time "$CHILD_MAX" \
				-H @- -o "$body_file" -w '%{http_code}' \
				"$url/v1/verdict" >"$code_file" &
		# curl's exit code tells DNS failure (6), refused connection (7) and
		# timeout (28) apart in the job log.
		wait "$!" && rc=0 || rc=$?
		IFS= read -r code 2>/dev/null <"$code_file" || true
		IFS= read -r -d '' body 2>/dev/null <"$body_file" || true

		if [ "$code" = 200 ]; then
			body=${body//[[:space:]]/}
			case $body in
			'{"verdict":"allow"}') verdict=allow ;;
			'{"verdict":"deny"}') verdict=deny ;;
			'{"verdict":"pending"}') verdict=pending ;;
			*) verdict=unknown ;;
			esac
		fi

		if [ "$verdict" = deny ]; then
			stop_runner
		fi

		if [ "$verdict" = allow ]; then
			/bin/rm -f "$token_file" "$body_file" "$code_file" 2>/dev/null || true
			if [ ! -e "$token_file" ]; then
				allowed=1
				printf 'scaler-job-started: job allowed\n' || true
				exit 0
			fi
			# Steps must never see the token, so keep waiting instead.
			state='allowed but cannot remove token file'
		else
			state="http ${code:-none}, verdict ${verdict:-none}, curl exit $rc"
		fi
	fi

	if [ "$state" != "$last" ]; then
		printf 'scaler-job-started: waiting for job approval (%s)\n' "$state" || true
		last=$state
	fi

	/bin/sleep "$POLL_INTERVAL" &
	wait "$!" || true
done
