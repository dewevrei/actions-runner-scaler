# actions-runner-scaler

Runs ephemeral GitHub Actions self-hosted runners as Docker containers on one macOS host and scales them with the number of jobs assigned to a repository's runner scale set. It uses GitHub's [`actions/scaleset`](https://github.com/actions/scaleset) library (v0.5.0).

> **Status:** under development. Real-GitHub behavior (job flow, OIDC, push, the job gate end to end, `delete-scaleset`, LaunchAgent restarts) has not been verified yet. Commands below describe the intended behavior and match the code's command-line interface.

## How it works

- One **pool** maps to one runner scale set registered on one repository. The scale set name is the `runs-on` label, `linux-arm64-deploy` by default. This version accepts exactly one pool.
- The scaler keeps `min(max, idle + assigned jobs)` runners. Each runner is one job: when the job ends the container exits, and the scaler removes its resources and its GitHub registration.
- Each runner gets its own resource set, created and removed together:
  - a runner container: non-root, all capabilities dropped, `no-new-privileges`, CPU/memory/PID limits, no host mounts, no Docker socket
  - a rootless BuildKit container: CPU/memory/PID limits, `seccomp=unconfined`, only `SETUID` and `SETGID` added back
  - a private bridge network that only those two containers join
- `docker buildx build` inside a job uses that BuildKit through a preconfigured default builder. The build cache starts empty and is deleted with the runner.
- A **job gate** blocks the first step of each job until the scaler allows it (see [Job gate](#job-gate)).
- The scaler passes no AWS, cluster, or GitHub credentials into containers. Jobs get AWS credentials themselves through GitHub OIDC.
- A launchd LaunchAgent starts the scaler at login and restarts it after a crash.

## Prerequisites

- macOS on Apple Silicon. Images are `linux/arm64` only.
- Docker Desktop. The scaler connects to `/var/run/docker.sock` and ignores `DOCKER_HOST`. Docker Desktop creates that socket when **Settings > Advanced > Allow the default Docker socket to be used** is on (changing it requires your password).
- Docker must not answer on `127.0.0.1:2375` (see [Docker TCP 2375](#docker-tcp-2375)).
- Go 1.27.3 or later, to build.
- A GitHub App installed on the target repository with these repository permissions:
  - **Administration: Read and write**: scale set, sessions, JIT runner configs, runner removal
  - **Actions: Read**: workflow run lookups for the job gate

  You need the App's client ID, the installation ID, and its private key file. Keep the key outside the repository, for example `~/.config/actions-runner-scaler/<app-key>.pem`, with mode `0600` or `0400`.

## Build

```sh
go build -o bin/actions-runner-scaler ./cmd/actions-runner-scaler
# optional version string
go build -ldflags "-X main.version=v0.1.0" -o bin/actions-runner-scaler ./cmd/actions-runner-scaler
bin/actions-runner-scaler version
```

Build the images:

```sh
image/build.sh
```

The script does two things:

- It pulls the BuildKit image pinned by index digest at the top of `image/build.sh` (`moby/buildkit:<version>-rootless@sha256:…`).
- It builds the runner image from `image/Dockerfile`: the official `actions-runner` base pinned by digest, plus the aws CLI v2, kubectl, the docker CLI, and buildx, with no Docker daemon.

It checks that both images are arm64 and prints the two config values:

```
# scaler config values
runner.image = "sha256:<runner-image-id>"
buildkit.image = "moby/buildkit:<version>-rootless@sha256:<index-digest>"
```

The scaler never pulls. Both images must stay in the local image store, so do not prune them. To change the BuildKit version, edit `BUILDKIT_IMAGE` in `image/build.sh` and run it again.

## Configuration

The default path is `~/.config/actions-runner-scaler/config.toml`. Every command that reads it also takes `-config <path>`. Start from `config.example.toml`.

The file is TOML:

- Unknown keys, malformed values, and out-of-range values are all reported together. A TOML syntax error is reported alone, with its line and column.
- On any of these errors the scaler enters error-wait.
- Secrets are referenced by path only.

| Key | Default | Allowed values |
|---|---|---|
| `github.app-client-id` | required | non-empty |
| `github.app-installation-id` | required | positive integer |
| `github.app-private-key-file` | required | absolute path to an unencrypted RSA PEM, at most 64 KiB, mode `0600` or `0400` (no group/other bits, no setuid/setgid/sticky) |
| `runner.image` | required | local image ID `sha256:<64 hex>` |
| `buildkit.image` | required | `moby/buildkit:<version>-rootless@sha256:<64 hex>` |
| `jobcheck.listen` | `127.0.0.1:8765` | `<loopback IP>:<port>` |
| `shutdown.drain-timeout` | `10m` | Go duration greater than 0 |
| `[[pools]]` | required | exactly one entry |
| `pools.name` | `linux-arm64-deploy` | 1 to 40 characters of `a-z`, `0-9`, `-`, starting with a letter or digit. Scale set name and `runs-on` label |
| `pools.repo-url` | required | `https://github.com/<owner>/<repo>`, no `.git` |
| `pools.max` | `2` | at least 1 |
| `pools.idle` | `0` | 0 to `max` |
| `pools.runner-cpus` | `1` | at least 0.01 |
| `pools.runner-memory` | `1g` | at least `256m`. Digits with an optional suffix `b`, `k`, `m`, `g` (binary multiples, either case). No suffix means bytes |
| `pools.runner-pids` | `512` | at least 64 |
| `pools.build-cpus` | `2` | at least 0.01 |
| `pools.build-memory` | `2g` | at least `256m` |
| `pools.build-pids` | `1024` | at least 64 |
| `pools.allowed-workflows` | required | one or more `.github/workflows/<file>.yml` or `.yaml` |
| `pools.allowed-events` | `["push"]` | one or more of `push`, `workflow_dispatch`, `schedule` |

The scaler applies no upper limits of its own. Docker rejects a CPU value above the Docker VM's CPU count when it creates the container, so such a value shows up as runner start failures. If `max × (runner-memory + build-memory)` exceeds Docker's memory, the scaler logs a warning at start and keeps running.

## Running

Run in the foreground:

```sh
bin/actions-runner-scaler run [-config <path>]
```

Logs go to stderr as JSON, with tokens and keys redacted. Only one scaler runs at a time: a second `run` exits 1 while another instance holds the lock.

Signals:

- Before the first session, the first SIGINT or SIGTERM exits at once.
- After that, the first signal starts a **drain**: no new runners, idle runners removed, busy runners finish, then exit 0. Runners still busy at `drain-timeout` are removed and their jobs fail, and the process still exits 0.
- A second signal removes what is left and exits at once.
- In error-wait, or during an error drain (one started by a permanent error), the first signal exits.

### status

```sh
bin/actions-runner-scaler status
```

`status` reads the owner-only socket `~/.local/state/actions-runner-scaler/status.sock`. Its output is in Korean:

| Output first line | Meaning | Exit |
|---|---|---|
| `상태: 시작 중` | starting, before the first session (including backoff) | 0 |
| `상태: 정상` | running | 0 |
| `상태: drain 중` | draining | 0 |
| `상태: 오류 대기` + `오류: <error> (<time>)` | error-wait: stopped on a permanent error until restarted | 1 |
| `실행 중 아님` | not running | 1 |
| `상태를 읽지 못함` | the socket accepted but no report could be read (timeout or malformed reply) | 1 |
| `상태: 알 수 없음 ("<state>")` | the scaler reported a state this binary does not know | 1 |

While running or draining, `status` prints one line per pool:

```
상태: 정상
linux-arm64-deploy: 실행 중 1, 대기 0, 연속 시작 실패 0
마지막 오류: 없음
```

- `실행 중` is busy runners, counting those that finished a job and are waiting for their container to exit.
- `대기` is idle and starting runners.
- `연속 시작 실패` is consecutive runner start failures. When it is non-zero, the last start error and its time follow in parentheses.
- `마지막 오류` is the last error and its time, or `없음` (none).

### LaunchAgent

```sh
bin/actions-runner-scaler service install [-config <path>]
bin/actions-runner-scaler service stop [-config <path>]
bin/actions-runner-scaler service uninstall [-config <path>]
```

Pass the same `-config` to all three. `stop` and `uninstall` read only `shutdown.drain-timeout` from it, to know how long to wait. If the config does not load, they log a warning and wait for the default 10 minutes. The wait follows the config you pass, not the one the running scaler loaded. Before you lower `drain-timeout` and reinstall, run `service stop` first, so the old, longer drain is not cut short by the wait.

`install` does the following:

- It validates the config. It refuses a binary built by `go run`, so run it from the binary you built.
- It writes `~/Library/LaunchAgents/io.github.dewevrei.actions-runner-scaler.plist`. The plist runs `<binary> run -config <absolute config path>` with `RunAtLoad`, `KeepAlive` → `SuccessfulExit` false, `ThrottleInterval` 60, and `ExitTimeOut` 60.
- It loads the plist into `gui/<uid>`. If the agent is already loaded, `install` first stops the running scaler the way `stop` does, then reloads it. Running `install` again is therefore a graceful restart.

The plist records the binary's absolute path, so keep the binary there. Changing `drain-timeout` needs no reinstall. The plist does not depend on it.

Restart behavior:

- launchd restarts the scaler after a non-zero exit, at most once every 60 s.
- A signal stop exits 0 and is not restarted.
- Error-wait keeps the process alive, so it does not loop.

Logs:

```
~/Library/Logs/actions-runner-scaler/stderr.log   # JSON log
~/Library/Logs/actions-runner-scaler/stdout.log
```

**Stop** (drains first):

```sh
bin/actions-runner-scaler service stop
```

`stop` sends SIGTERM to the scaler and waits until it has exited. It waits at most `drain-timeout` + 90 s and prints progress every 10 s. It gets the scaler's pid from the status socket, so it never signals another process. If the scaler is already draining, it sends no second signal, because a second signal cuts the drain short. It only waits. If nothing is running, it says so and exits 0. If the wait runs out, the scaler keeps draining and `stop` exits 1. Check with `status`, then run `stop` again later.

The agent stays loaded. The scaler exited 0, so launchd does not restart it. It starts again at the next login, or now with:

```sh
launchctl kickstart gui/$(id -u)/io.github.dewevrei.actions-runner-scaler
```

`uninstall` stops the scaler the same way, then unloads the agent and removes the plist. If the scaler does not exit in time, `uninstall` leaves the agent and the plist in place and exits 1, and the scaler keeps draining. `install` and `uninstall` also stop with an error if the status socket accepts a connection but does not answer. In that case the scaler is hung. Unload it with `launchctl bootout gui/$(id -u)/io.github.dewevrei.actions-runner-scaler`, which kills it 60 s later, and then run the command again.

**Do not use `launchctl bootout` or `kickstart -k` for a graceful stop.** Since macOS 26.4, launchd caps `ExitTimeOut` at 60 s, whatever the plist says (Apple Developer Forums thread 854315). So when launchd stops the scaler, it sends SIGKILL 60 s after SIGTERM. That covers `bootout`, `kickstart -k`, logout, restart and shutdown. A drain longer than 60 s is cut off. The job that was running is lost: GitHub marks it failed about 10 minutes later. Its leftover containers and registration are removed at the next start. Run `service stop` before you restart or shut down the Mac.

**Restart** after fixing the cause of error-wait or editing the config:

```sh
bin/actions-runner-scaler service stop
launchctl kickstart gui/$(id -u)/io.github.dewevrei.actions-runner-scaler
```

Or run `service install` again. In error-wait, the scaler exits on the first signal, so `stop` returns at once. `kickstart -k` restarts in one step, but it cuts a running drain at 60 s (see above).

### delete-scaleset

```sh
bin/actions-runner-scaler delete-scaleset [-config <path>] <pool name>
```

This deletes the pool's scale set and its runner registrations. The pool name must be in the config. If no scale set has that name, it says so and exits 0.

It deletes nothing and exits 1 in three cases:

- A scaler holds the lock, including one in error-wait. Stop it first with `service stop`.
- More than one scale set has the pool's name. Delete the extras so only one is left, then run again.
- A runner container of that pool is still alive. The command lists those runners. Remove the containers, then run again:

  ```sh
  docker ps -aq --filter label=managed-by=actions-runner-scaler --filter label=kind=runner | xargs docker rm -f
  ```

  The BuildKit containers and networks left behind are removed at the next scaler start.

A normal stop never deletes the scale set.

## Docker TCP 2375

If Docker answers `GET /_ping` on `127.0.0.1:2375`, any container could control the host's Docker through `host.docker.internal`. The scaler checks this at start and enters error-wait with that message.

To turn the exposure off:

1. If Docker Desktop shows **Settings > General > Expose daemon on tcp://localhost:2375 without TLS**, turn it off and apply. Docker documents this option for the Windows Hyper-V backend.
2. Stop anything else listening on that port, such as a `socat` forward of the Docker socket:

   ```sh
   lsof -nP -iTCP:2375 -sTCP:LISTEN
   curl -s --max-time 2 http://127.0.0.1:2375/_ping   # must not print OK
   ```

3. Restart the scaler.

## Updating the runner version

Scale sets are created with runner self-update disabled, so the runner version is fixed by the image. GitHub stops assigning jobs to a runner version 63 days after a newer version is made available. That date can differ from the release date. Look up the exact date (requires admin access to the repository):

```sh
gh api repos/<owner>/<repo>/actions/runners/deprecations/<version> --jq .runtime_deprecates_at
```

Before that date:

1. In `image/Dockerfile`, change the `FROM ghcr.io/actions/actions-runner:<version>@sha256:<digest>` line. Get the index digest with `docker buildx imagetools inspect ghcr.io/actions/actions-runner:<version>`.
2. Re-check the job-started hook (`image/job-started.sh`) against that runner version: how the runner invokes the hook, its environment, and its signals. The hook was written against the version currently pinned.
3. Run `image/build.sh` and put the new `runner.image` in the config.
4. Restart the scaler.

## Job gate

The runner image sets a job-started hook. The hook asks the scaler for a verdict at `http://host.docker.internal:<jobcheck port>/v1/verdict`, using a per-runner token. It lets the job continue only on `allow`. On any other answer it keeps waiting and never lets a step run, including `if: always()` steps.

When the scaler receives the job's `JobStarted` message, it allows the job only if all of these hold:

- the repository is the pool's `repo-url`
- the workflow file is in `allowed-workflows` and is the run's own top-level file, so reusable workflow calls are refused
- the ref is exactly `refs/heads/main` and the run's head branch is `main` of the same repository, so forks and a `main` tag are refused
- the event is in `allowed-events`
- the run references no other workflows
- no newer `main` run of the same workflow exists (the **latest-run condition**)

On a denial, or when no allow arrives within 180 s of the runner turning busy, the scaler removes the runner's containers. The job then fails without running any step.

**Rollback:** because of the latest-run condition, re-running an older run is refused once a newer `main` run of that workflow exists. To roll back, push a revert commit to `main` and let its new run deploy.

## Workflow requirements

```yaml
on:
  push:
    branches: [main]
permissions:
  id-token: write      # for AWS credentials via GitHub OIDC
  contents: read
jobs:
  deploy:
    runs-on: linux-arm64-deploy   # the pool name
    steps:
      - uses: actions/checkout@v4
      # ... aws-actions/configure-aws-credentials with role-to-assume ...
      - run: docker buildx build --push -t <registry>/<image>:<tag> .
```

- The workflow file must be listed in `allowed-workflows`, and the triggering event must be in `allowed-events`.
- Do not call reusable workflows from it.
- Build and push with `docker buildx build --push`. The runner has no Docker daemon, and its default builder already points at the runner's BuildKit.
- The runner has the aws CLI v2, kubectl, git, the docker CLI, and buildx (arm64).

## Troubleshooting

- **Start with `status`.** In error-wait, the `오류:` line holds every config error or the permanent error that stopped startup (missing image, Docker TCP 2375, authentication, scale set). The scaler makes no GitHub or Docker calls in this state. Fix the cause, then restart with `service stop` and `launchctl kickstart` (see Restart).
- **Read the log:** `~/Library/Logs/actions-runner-scaler/stderr.log`, one JSON object per line, for example `tail -f … | jq .`.
- **`실행 중 아님`:** the agent is not loaded or the process exited. Check the log and `launchctl print gui/$(id -u)/io.github.dewevrei.actions-runner-scaler`.
- **A non-zero `연속 시작 실패`:** runner starts are failing, for example because an image was removed, a CPU or memory value is too large for Docker, or JIT issuance failed. After 3 failures in a row, starts back off from 10 s up to 5 min.
- **A job sits at "Set up job" and then fails:** the gate denied it or never allowed it. Search the log for the runner name to see the reason.
- **Leftover containers after a crash:** they remain until the next start. At start the scaler removes every container and network it labeled, and stale registrations of the pool.
