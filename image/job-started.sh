#!/bin/bash
PATH=/usr/bin:/bin
# Job-started hook. The runner runs it as `bash -e <path>` before any step.
#
# A hook that ends without success still lets always() and failure() steps
# run, so this hook never ends on its own except on an allow verdict. On any
# other end (signal, unexpected error) the trap SIGKILLs the runner processes
# first so the container exits before another step can start.
#
# The one exception is a deny verdict: the hook SIGKILLs only the Worker
# (START_PID, the job's process) and ends with 1, leaving the Listener and
# its ancestors alive. With the Worker gone no step can run, whatever the
# Listener does next. The Listener sees its Worker end with a code that is
# not a job result and reports the job failed itself (actions/runner
# JobDispatcher RunAsync, worker-finished branch, ForceFailJob), then the
# ephemeral runner exits and the container stops. The Listener must stay
# alive and must not be asked to shut down: once shutting down it skips
# the report for run-service jobs (CompleteJobRequestAsync), and GitHub
# then holds the job for about ten minutes. If the Worker cannot be
# killed, the hook kills every runner process as on any other end. The
# scaler removes the container if it has not stopped 30 seconds after the
# deny. Exiting 1 alone is not enough: the runner adds this hook as an
# always() pre-job step in the job's step list, so always() and failure()
# steps could still run after it (read from actions/runner JobExtension.cs,
# not tested).
#
# Before the first verdict query the hook reports the job's event: it sends
# the event file to POST /v1/event and the scaler judges it (0002 SPEC R139,
# R140). An answer of allow or deny is handled as from the query; no answer
# or 401 sends the report again after POLL_INTERVAL; any other answer
# switches to GET /v1/verdict for good, so a large body is never resent.
#
# The scaler address, runner name and token come only from files written by
# the entrypoint, and every command is called by absolute path, so
# environment variables a job can influence cannot redirect the query. The
# only values read from the environment are GITHUB_EVENT_NAME and
# GITHUB_EVENT_PATH, once at start: the runner sets them for the hook, no
# job code runs before it and workflow env: does not reach it.
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
# jq of the diagnostic variant; the normal hook never runs it.
JQ=/usr/bin/jq

# Read once, before anything else runs; later changes are never seen.
event_name=${GITHUB_EVENT_NAME-}
event_path=${GITHUB_EVENT_PATH-}
event_name_set=${GITHUB_EVENT_NAME+set}
event_path_set=${GITHUB_EVENT_PATH+set}
# Keep the name only if it matches ^[a-z_]{1,64}$, so no header can be
# injected. The letters are listed one by one: a range such as [a-z] can
# match other characters in some locales.
case $event_name in
'' | *[!abcdefghijklmnopqrstuvwxyz_]*) event_name='' ;;
esac
if [ "${#event_name}" -gt 64 ]; then
	event_name=''
fi
if [ -n "$event_name" ]; then
	event_header="X-Event-Name: $event_name"
else
	# curl drops a header written as "Name:" and sends "Name;" empty.
	event_header='X-Event-Name;'
fi
# Only an absolute path: curl reads "@-" from stdin, which holds the token.
case $event_path in /*) ;; *) event_path='' ;; esac

token_file=$STATE_DIR/token
endpoint_file=$STATE_DIR/endpoint
body_file=$STATE_DIR/verdict.body
code_file=$STATE_DIR/verdict.code

allowed=0
# released marks the end after a deny killed the Worker; the EXIT trap then
# kills nothing else.
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
	kill_runner
	exit 1
}

# kill_worker handles a deny verdict: it SIGKILLs the Worker alone and ends
# the hook. When the Worker cannot be killed it kills every runner process.
kill_worker() {
	# As in kill_runner, no signal may cut this short.
	trap '' INT TERM HUP QUIT USR1 USR2 ALRM VTALRM PROF XCPU XFSZ SYS TRAP ABRT BUS FPE ILL SEGV
	case $START_PID in
	'' | *[!0-9]*) ;;
	*)
		if [ "$START_PID" -gt 1 ] && [ "$START_PID" -ne "$$" ] &&
			kill -KILL "$START_PID" 2>/dev/null; then
			# Kill before logging: the Worker held the read end of our
			# output pipes.
			released=1
			printf 'scaler-job-started: kill -KILL %s\n' "$START_PID" >&2 || true
			printf 'scaler-job-started: job denied; killed the worker\n' || true
			exit 1
		fi
		;;
	esac
	printf 'scaler-job-started: job denied; cannot kill the worker, killing the runner\n' || true
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

# event_readable succeeds when the event path names a regular file the hook
# can read now.
event_readable() {
	[ -n "$event_path" ] && [ -f "$event_path" ] && [ -r "$event_path" ]
}

# compare_program prints, for each pair of the repository IDs the scaler
# compares (0002 SPEC R126, R127), one word: same, different, missing (a
# level absent or not an object) or null. jq compares numbers as doubles,
# which only these words depend on; the scaler compares the raw integers.
# shellcheck disable=SC2016 # jq variables, not shell ones
compare_program='
def look($p): reduce $p[] as $k ({s: "value", v: .};
	if .s != "value" then .
	elif (.v | type) == "object" and (.v | has($k)) then
		.v = .v[$k] | if .v == null then .s = "null" else . end
	else .s = "missing" end);
def word($a; $b):
	if $a.s == "missing" or $b.s == "missing" then "missing"
	elif $a.s == "null" or $b.s == "null" then "null"
	elif $a.v == $b.v then "same" else "different" end;
def compare($ps): . as $doc | range(0; $ps | length) as $i |
	range($i + 1; $ps | length) as $j |
	"compare \($ps[$i] | join(".")) \($ps[$j] | join(".")): \(word($doc | look($ps[$i]); $doc | look($ps[$j])))";
input |
	compare([["pull_request", "head", "repo", "id"], ["pull_request", "base", "repo", "id"], ["repository", "id"]]),
	compare([["workflow_run", "head_repository", "id"], ["workflow_run", "repository", "id"], ["repository", "id"]])
'

# Diagnostic variant (build.sh --record): how the runner started the hook,
# which variable names it passed, the ignored and blocked signal masks
# (INT and TERM must be in neither; bash cannot trap a signal ignored at
# start, and PIPE shows as ignored by this hook), whether the event
# variables are set, the event file size and the ID comparison words.
# After the first event report it adds that request's status and time.
# Values (IDs, names, the event name) are never written.
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
		printf 'event name: %s\n' "${event_name_set:-unset}"
		printf 'event path: %s\n' "${event_path_set:-unset}"
		if event_readable; then
			printf 'event file: readable\n'
			size=$(/usr/bin/wc -c <"$event_path") || size=''
			printf 'event file bytes: %s\n' "${size//[[:space:]]/}"
			/usr/bin/env -i "$JQ" -n -r "$compare_program" <"$event_path" ||
				printf 'compare: jq failed\n'
		else
			printf 'event file: unreadable\n'
		fi
	} 2>/dev/null >"$STATE_DIR/hook-record"
}

if [ "$RECORD" = 1 ]; then
	record || true
fi

last=''
# event: report the event with POST /v1/event; verdict: query it (0001 R66).
phase=event
recorded_report=0
while :; do
	url=''
	name=''
	token=''
	code=''
	total=''
	body=''
	verdict=''
	asked=$phase

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
		if [ "$asked" = event ]; then
			# A missing or unreadable file sends an empty body. If the file
			# goes away after this check, curl either fails without a status
			# (000), and the next report sends an empty body, or sends an
			# empty body itself (unverified which; both deny, R139, R140).
			event_file=/dev/null
			if event_readable; then
				event_file=$event_path
			fi
			printf 'Authorization: Bearer %s\nX-Runner-Name: %s\n%s\n' "$token" "$name" "$event_header" 2>/dev/null |
				/usr/bin/env -i /usr/bin/curl -q --noproxy '*' -s -X POST \
					--connect-timeout "$CONNECT_TIMEOUT" --max-time "$CHILD_MAX" \
					--data-binary "@$event_file" \
					-H @- -o "$body_file" -w '%{http_code} %{time_total}' \
					"$url/v1/event" >"$code_file" &
		else
			printf 'Authorization: Bearer %s\nX-Runner-Name: %s\n' "$token" "$name" 2>/dev/null |
				/usr/bin/env -i /usr/bin/curl -q --noproxy '*' -s \
					--connect-timeout "$CONNECT_TIMEOUT" --max-time "$CHILD_MAX" \
					-H @- -o "$body_file" -w '%{http_code}' \
					"$url/v1/verdict" >"$code_file" &
		fi
		# curl's exit code tells DNS failure (6), refused connection (7) and
		# timeout (28) apart in the job log.
		wait "$!" && rc=0 || rc=$?
		read -r code total 2>/dev/null <"$code_file" || true
		IFS= read -r -d '' body 2>/dev/null <"$body_file" || true

		if [ "$RECORD" = 1 ] && [ "$asked" = event ] && [ "$recorded_report" = 0 ]; then
			recorded_report=1
			printf 'first event report: http %s, time_total %s, curl exit %s\n' \
				"${code:-none}" "${total:-none}" "$rc" 2>/dev/null >>"$STATE_DIR/hook-record" || true
		fi

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
			kill_worker
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

		# No answer (000, or no status at all) or 401 sends the report again;
		# any other answer leaves only the verdict query (0002 SPEC R140).
		if [ "$asked" = event ]; then
			case $code in
			'' | 000 | 401) ;;
			*) phase=verdict ;;
			esac
		fi
	fi

	if [ "$asked $state" != "$last" ]; then
		if [ "$asked" = event ]; then
			printf 'scaler-job-started: reporting the job event (%s)\n' "$state" || true
		else
			printf 'scaler-job-started: waiting for job approval (%s)\n' "$state" || true
		fi
		last="$asked $state"
	fi

	/bin/sleep "$POLL_INTERVAL" &
	wait "$!" || true
done
