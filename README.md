# actions-runner-scaler

Runs ephemeral GitHub Actions self-hosted runners as Docker containers on one macOS host and scales them with the number of jobs assigned to runner scale sets registered on one or more GitHub organizations. It uses GitHub's [`actions/scaleset`](https://github.com/actions/scaleset) library (v0.5.0).

> **Status:** under development. Real-GitHub behavior (organization registration, the job gate end to end, waiting jobs, `delete-scaleset`, LaunchAgent restarts) has not been verified yet. Commands below describe the intended behavior and match the code's command-line interface.

## How it works

- One **target** is one GitHub organization (`[[targets]]` in the config). For each target the scaler registers a runner scale set at the organization level, in the target's runner group. The scale set name is the `runs-on` label, `linux-arm64-deploy` by default (`scaling.label`). The runner group's repository access on GitHub decides which repositories can use it, so adding a repository there needs no change to the scaler.
- Each target keeps about as many runners as it has assigned jobs plus its `idle` setting, never more than its `max-runners`. Each runner is one job: when the job ends the container exits, and the scaler removes its resources and its GitHub registration.
- All targets share one **server limit** on the number of runners (see [Server limit](#server-limit)). Jobs that do not fit wait in GitHub's queue and get a slot as one frees up, the one GitHub queued first going first, whichever organization it belongs to.
- Each runner gets its own resource set, created and removed together:
  - a runner container: non-root, all capabilities dropped, `no-new-privileges`, CPU/memory/PID limits, no host mounts, no Docker socket
  - a rootless BuildKit container: CPU/memory/PID limits, `seccomp=unconfined`, only `SETUID` and `SETGID` added back
  - a private bridge network that only those two containers join
- `docker buildx build` inside a job uses that BuildKit through a preconfigured default builder. The build cache starts empty and is deleted with the runner.
- A **job gate** blocks the first step of each job until the scaler allows it. It refuses jobs that come from fork pull requests (see [Job gate](#job-gate)).
- The scaler passes no AWS, cluster, or GitHub credentials into containers. Jobs get AWS credentials themselves through GitHub OIDC.
- A launchd LaunchAgent starts the scaler at login and restarts it after a crash.

## Prerequisites

- macOS on Apple Silicon. Images are `linux/arm64` only.
- Docker Desktop. The scaler connects to `/var/run/docker.sock` and ignores `DOCKER_HOST`. Docker Desktop creates that socket when **Settings > Advanced > Allow the default Docker socket to be used** is on (changing it requires your password).
- Docker must not answer on `127.0.0.1:2375` (see [Docker TCP 2375](#docker-tcp-2375)).
- Go 1.27.3 or later, to build.
- A GitHub App with the **Organization** permission **Self-hosted runners: Read and write**. You need the App's client ID and its private key file. Keep the key outside the repository, for example `~/.config/actions-runner-scaler/<app-key>.pem`, with mode `0600` or `0400`. There is no installation ID to configure: the scaler looks up the App's installation on each target at start. The repository permissions the 0001 version used (Administration, Actions) are not used for organization targets.
- The setup on GitHub described in [GitHub setup](#github-setup).

## GitHub setup

An owner of each organization does this. The scaler cannot do it, and a target that is not set up stops with an error in `status` (see [Troubleshooting](#troubleshooting)).

1. **Install the App on the organization.** If you add the Self-hosted runners permission to an App that is already installed, the owner must approve the changed permissions. A missing installation shows as `GitHub App is not installed on organization <org>`, a missing permission as `GitHub App installation on organization <org> lacks organization_self_hosted_runners: write`.
2. **Choose the runner group.** By default (`runner-group = ""`) the scale set goes into the default runner group, and `runner-group = "<name>"` puts it into the group of that name, which must exist. In the group's settings, choose the repositories that may use the runners. A runner group serves only private repositories unless the group allows **public repositories**.
3. **Public repositories need extra care.** GitHub advises against self-hosted runners for public repositories. If you allow them anyway, the scaler refuses only jobs from fork pull requests (see [Job gate](#job-gate) for what it does not stop). Use GitHub's own settings for the rest: require approval for workflow runs from fork pull requests, and, where your plan offers workflow execution protections, limit the events a repository may run, for example `pull_request_target` and `workflow_run`.
4. **Changing the group or leaving an organization.** Scale sets are not deleted when the scaler stops, and a scale set name is unique only within one runner group. Before you change `targets.runner-group` or remove a target from the config, stop the scaler and run [`delete-scaleset`](#delete-scaleset) with the old configuration. Otherwise the old scale set stays on GitHub with no scaler listening to it, and jobs for the label could be assigned to it and wait.

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
| `github.app-private-key-file` | required | absolute path to an unencrypted RSA PEM, at most 64 KiB, mode `0600` or `0400` (no group/other bits, no setuid/setgid/sticky) |
| `runner.image` | required | local image ID `sha256:<64 hex>` |
| `runner.cpus` | `1` | at least 0.01 |
| `runner.memory` | `1g` | at least `256m`. Digits with an optional suffix `b`, `k`, `m`, `g` (binary multiples, either case). No suffix means bytes |
| `runner.pids` | `512` | at least 64 |
| `buildkit.image` | required | `moby/buildkit:<version>-rootless@sha256:<64 hex>` |
| `buildkit.cpus` | `2` | at least 0.01 |
| `buildkit.memory` | `2g` | at least `256m` |
| `buildkit.pids` | `1024` | at least 64 |
| `scaling.label` | `linux-arm64-deploy` | 1 to 40 characters of `a-z`, `0-9`, `-`, starting with a letter or digit. Scale set name and `runs-on` label, the same for every target |
| `jobcheck.listen` | `127.0.0.1:8765` | `<loopback IP>:<port>` |
| `shutdown.drain-timeout` | `10m` | Go duration greater than 0 |
| `[[targets]]` | required | one entry per organization, no upper limit |
| `targets.url` | required | `https://github.com/<org>`, no trailing `/` and no `.git`. Organization names that differ only in case count as the same organization, and listing one twice is an error |
| `targets.max-runners` | `2` | at least 1. The most runners this target has at once |
| `targets.runner-group` | `""` | empty for the default runner group, otherwise the name of an existing runner group (1 to 255 characters) |
| `targets.idle` | `0` | 0 to this target's `max-runners`. Runners kept waiting for a job |

The scaler applies no upper limits of its own to `cpus` and `memory`. A CPU value above the Docker VM's CPU count would make Docker reject every container, so the scaler stops it earlier, with `server runner limit is 0` (see [Server limit](#server-limit)).

### Server limit

The scaler never runs more runners, over all targets together, than the **server limit**. It is not a config key. At start the scaler asks Docker for its CPU count and memory and computes

```
min(floor(CPUs / (runner.cpus + buildkit.cpus)), floor(memory / (runner.memory + buildkit.memory)))
```

For example, with Docker at 10 CPUs and 8 GiB and the defaults (one runner reserves 3 CPUs and 3 GiB), the limit is min(3, 2) = 2. The start-up log has an INFO line `server runner limit` with `limit`, `ncpu`, `memTotal`, `runnerCPUs` and `runnerMemory`, and `status` shows the limit on its `전체` line.

- The limit is computed once, at start. After you change the CPUs or memory given to Docker, restart the scaler.
- To raise the limit, give Docker more CPU or memory, or lower the per-runner `cpus` and `memory` of `[runner]` and `[buildkit]`. Docker's limits are caps, not reservations, but the scaler counts every runner at its caps, so the runners can never use more than Docker has.
- If the limit is 0 the scaler enters error-wait with `server runner limit is 0: one runner reserves ...`, naming the four keys and their values.
- If the `idle` of all targets adds up to more than the limit, the scaler enters error-wait with `idle of all targets (<sum>) exceeds the server runner limit (<limit>)`. If the sum equals the limit while another target has `idle = 0`, it logs the WARN `idle runners of other targets take every slot` and keeps running.
- The `max-runners` of all targets may add up to more than the limit. The scaler logs the INFO line `max-runners of all targets exceed the server limit` and keeps running.

### Migrating a 0001 config

The 0001 format (one `[[pools]]` entry with a repository URL and a workflow allow-list) is rejected: the scaler enters error-wait, and the error says what to move. Nothing is converted silently, because `allowed-workflows` and `allowed-events` would otherwise be ignored without your knowing.

| 0001 | Now |
|---|---|
| `[[pools]]` | removed. The error reads `pools: [[pools]] is the 0001 format; move max to [[targets]] max-runners, idle to [[targets]] idle, runner-*/build-* to [runner]/[buildkit] cpus, memory, pids, name to [scaling] label, and set [[targets]] url = "https://github.com/<org>"; allowed-workflows and allowed-events no longer exist` |
| `pools.name` | `scaling.label` |
| `pools.repo-url` | `targets.url`, now an organization URL |
| `pools.max` | `targets.max-runners` |
| `pools.idle` | `targets.idle` |
| `pools.runner-cpus`, `runner-memory`, `runner-pids` | `runner.cpus`, `runner.memory`, `runner.pids` |
| `pools.build-cpus`, `build-memory`, `build-pids` | `buildkit.cpus`, `buildkit.memory`, `buildkit.pids` |
| `pools.allowed-workflows`, `pools.allowed-events` | removed, with no replacement (see [Job gate](#job-gate)) |
| `github.app-installation-id` | removed. The error reads `github.app-installation-id: no longer used; the installation is looked up for each target` |

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
- When the whole scaler is in error-wait (a config, Docker, lock or job gate listen error), the first signal exits.
- A single target can also stop on its own (see [Target errors](#target-errors)). If a signal arrives while it is draining, its busy runners are waited for like those of the other targets.

### status

```sh
bin/actions-runner-scaler status
```

`status` reads the owner-only socket `~/.local/state/actions-runner-scaler/status.sock`. Its output is in Korean:

| Output first line | Meaning | Exit |
|---|---|---|
| `상태: 시작 중` | starting, before the first session (including backoff) | 0 |
| `상태: 정상` | running, and at least one target is running or starting | 0 |
| `상태: 정상 target 없음` | running, but every target is in a target error (see [Target errors](#target-errors)), so no job is taken | 1 |
| `상태: drain 중` | draining | 0 |
| `상태: 오류 대기` + `오류: <error> (<time>)` | error-wait: the whole scaler stopped on a permanent error until restarted | 1 |
| `실행 중 아님` | not running | 1 |
| `상태를 읽지 못함` | the socket accepted but no report could be read (timeout or malformed reply) | 1 |
| `상태: 알 수 없음 ("<state>")` | the scaler reported a state this binary does not know | 1 |

Unless the scaler is in error-wait, `status` prints one line per target, then (when running or draining) a total line and the last error:

```
상태: 정상
https://github.com/<org>: 정상, 실행 중 1, 대기 0, 대기 job 0, 최대 2, 연속 시작 실패 0
전체: 실행 중 1, 대기 0, 최대 2
마지막 오류: 없음
```

- The target state is `시작 중` (starting), `정상` (running), `오류 drain 중` or `오류 대기` (see [Target errors](#target-errors)).
- `실행 중` is busy runners, counting those that finished a job and are waiting for their container to exit.
- `대기` is idle and starting runners.
- `대기 job` is jobs of this target that are waiting for a free slot.
- `최대` on a target line is that target's `max-runners`.
- `연속 시작 실패` is consecutive runner start failures. When it is non-zero, the last start error and its time follow in parentheses.
- In `오류 drain 중` and `오류 대기` the line continues with `, 오류: <error> (<time>)`, and in `오류 대기` with `, 다음 시도 <time>`.
- `전체` adds up `실행 중` and `대기` over all targets, and `최대` is the server limit (`-` until it is computed).
- `마지막 오류` is the last error of the whole scaler and its time, or `없음` (none).

A target in error-wait looks like this:

```
https://github.com/<org>: 오류 대기, 실행 중 0, 대기 0, 대기 job 0, 최대 2, 연속 시작 실패 0, 오류: GitHub App is not installed on organization <org> (<time>), 다음 시도 <time>
```

### Target errors

A permanent error of one target's GitHub calls stops only that target, so one organization's setup problem does not stop the others. Examples are an organization without the App installed, a missing permission, or a runner group that does not exist.

- At start, the target goes straight to `오류 대기`.
- While running, the target goes to `오류 drain 중`: it starts no runners, its idle runners are removed, and its busy runners finish (up to `shutdown.drain-timeout`). Then it goes to `오류 대기`.
- In `오류 대기` the scaler retries the target's start-up calls, first after 30 s, then at doubling intervals up to every 30 minutes, with no limit on the number of tries. When the cause is fixed on GitHub, the target returns to `정상` without a restart. `status` shows the time of the next try.
- A signal drain stops the retries.

Config errors, Docker problems, the lock and the job gate's listen address stop the whole scaler instead (`상태: 오류 대기`), and need a restart after you fix them.

### LaunchAgent

```sh
bin/actions-runner-scaler service install [-config <path>]
bin/actions-runner-scaler service stop [-config <path>]
bin/actions-runner-scaler service uninstall [-config <path>]
```

`stop` and `uninstall` read only `shutdown.drain-timeout` from the config, to know how long to wait. Without `-config`, they use the config named in the installed plist, or `~/.config/actions-runner-scaler/config.toml` if there is no plist. If the config does not load, they log a warning and wait for the default 10 minutes. `install` waits for the `drain-timeout` of the config it installs, not the one the running scaler loaded. Before you lower `drain-timeout` and reinstall, run `service stop` first, so the old, longer drain is not cut short by the wait.

Only one of `install`, `stop` and `uninstall` runs at a time. They share a lock file, `~/.local/state/actions-runner-scaler/stop.lock`. A second command prints `another service command is running` and exits 1. Without the lock, two commands could both signal the scaler, and a second signal cuts the drain short.

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

`stop` sends SIGTERM to the scaler and waits until it has exited. It waits at most `drain-timeout` + 90 s and prints progress every 10 s. It only signals the process serving the status socket. It reads that process's pid from the socket and checks it again after it starts watching the process. Right before signaling, it skips the signal if the process has already exited, because its pid could then belong to another process. If the scaler is already draining, it sends no second signal, because a second signal cuts the drain short. It only waits. If nothing is running, it says so and exits 0. If the wait runs out, the scaler keeps draining and `stop` exits 1. Check with `status`, then run `stop` again later.

The agent stays loaded. The scaler exited 0, so launchd does not restart it. It starts again at the next login, or now with:

```sh
launchctl kickstart gui/$(id -u)/io.github.dewevrei.actions-runner-scaler
```

`uninstall` stops the scaler the same way, then unloads the agent and removes the plist. If the scaler does not exit in time, `uninstall` leaves the agent and the plist in place and exits 1, and the scaler keeps draining. After a drain that `install` or `uninstall` started, the scaler stays stopped until the next login, `launchctl kickstart`, or the next run of that command. If the scaler stopped but unloading the agent was not confirmed, the error says so: the old agent is still loaded and starts the scaler again at the next login or on `kickstart`. `install` and `uninstall` also stop with an error if the status socket accepts a connection but does not answer. In that case the scaler is hung. Unload it with `launchctl bootout gui/$(id -u)/io.github.dewevrei.actions-runner-scaler`, which kills it 60 s later, and then run the command again.

**Do not use `launchctl bootout` or `kickstart -k` for a graceful stop.** Since macOS 26.4, launchd caps `ExitTimeOut` at 60 s, whatever the plist says (Apple Developer Forums thread 854315). So when launchd stops the scaler, it sends SIGKILL 60 s after SIGTERM. We saw this with `bootout`, and Apple's answer says it also applies to logout, restart and shutdown. `kickstart -k` and `launchctl stop` are reported to SIGKILL at 60 s too, but we have not tested them. A drain longer than 60 s is cut off. The job that was running is lost. GitHub marks it failed, probably about 10 minutes later (observed for a similar case, not for this one). Its leftover containers and registration are removed at the next start. Run `service stop` before you restart or shut down the Mac.

**Restart** after fixing the cause of error-wait or editing the config:

```sh
bin/actions-runner-scaler service stop
launchctl kickstart gui/$(id -u)/io.github.dewevrei.actions-runner-scaler
```

Or run `service install` again. In error-wait, the scaler exits on the first signal, so `stop` returns at once. `kickstart -k` restarts in one step, but it is reported to cut a running drain at 60 s (see above).

### delete-scaleset

```sh
bin/actions-runner-scaler delete-scaleset [-config <path>] <target URL>
```

This deletes one target's scale set and its runner registrations. `<target URL>` is the `targets.url` value, for example `https://github.com/<org>`; organization names are compared without case. The command looks up the App's installation on that organization and checks its permission, finds the runner group from `targets.runner-group`, then deletes the scale set named `scaling.label` in that group together with the registrations of its runners. Other targets are not touched. If no scale set has that name, it says so and exits 0.

Run it with the old configuration before you change `targets.runner-group` or remove a target from the config (see [GitHub setup](#github-setup)).

**It needs the whole scaler stopped.** The command takes the same lock as `run`, so one target's scale set cannot be deleted while the scaler runs. Stop it with `service stop`, which drains every target: no new jobs are taken, and the busy jobs of the other organizations are waited for too, for up to `shutdown.drain-timeout`. Start the scaler again afterwards (see [Restart](#restart-after-fixing-the-cause-of-error-wait-or-editing-the-config)).

It deletes nothing and exits 1 in these cases:

- A scaler holds the lock, including one in error-wait. Stop it first with `service stop`.
- The URL is not in the config, or the installation lookup, the permission check or the runner group lookup fails (for example `GitHub App is not installed on organization <org>`). The error goes to stderr.
- More than one scale set has the label's name. Delete the extras so only one is left, then run again.
- A runner container of that scale set is still alive. The command lists those runners. Remove the containers, then run again:

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

The runner image sets a job-started hook. Before any step runs, the hook sends the job's event name and event file (`GITHUB_EVENT_NAME` and the file `GITHUB_EVENT_PATH` points to) to the scaler at `http://host.docker.internal:<jobcheck port>/v1/event`, using a per-runner token. The scaler decides from that report alone and makes no request to GitHub. The hook lets the job continue only on `allow`. On any other answer it keeps waiting and never lets a step run, including `if: always()` steps.

**What is refused.** The scaler refuses jobs that come from a fork pull request, by comparing repository IDs in the event file:

- the event has a `pull_request` object (whatever the event name) and its head repository, its base repository and the run's repository are not all the same repository
- the event has a `workflow_run` object and the run's head repository and the run's repository are not the same repository. This covers a workflow started by the run of a fork pull request
- the event name is `pull_request`, `pull_request_target`, `pull_request_review` or `pull_request_review_comment` but the event has no `pull_request` object, or it is `workflow_run` without a `workflow_run` object
- a repository ID that is compared is missing, `null`, or not a positive integer
- the report is not exactly one JSON object, its event name is not 1 to 64 lowercase letters and `_`, or its body is larger than 32 MiB

Everything else is allowed: any branch, tag or event, including manual runs and pull requests from branches of the same repository.

On a denial the scaler logs `job denied; waiting for the runner to stop` with `runner`, `event`, `reason` and, when the event names it, `head_repo`. The `reason` is one of `pull-request-from-other-repo`, `workflow-run-from-other-repo`, `pull-request-repo-id-invalid`, `workflow-run-repo-id-invalid`, `pull-request-missing`, `workflow-run-missing`, `payload-not-one-object`, `event-name-invalid` or `payload-too-large`. The job then fails without running any step after "Set up job", and the runner's containers are removed. The same happens when no report arrives within 180 s of the runner turning busy.

**What the gate does not stop.** It looks at neither the person who started a job nor the branch. These are allowed, and are left to GitHub's settings and the repository's access rules (see [GitHub setup](#github-setup)):

- On public repositories: a comment (`issue_comment`, for example a `/deploy` command) on a fork pull request, when the workflow then checks out the pull request; a chain of `workflow_run` workflows that starts from a fork pull request; a review from an outsider on a same-repository pull request; events anyone can cause, such as `watch`, `fork` and `discussion_comment`.
- A re-run of an older run of the default branch by someone with write access. It runs the old commit and, if the AWS trust policy accepts that ref, gets the AWS role.
- In the same repository, `pull_request_target` and `workflow_run` jobs, which run on the default branch.

Whether a job gets the AWS role is decided by the AWS trust policy, not by the scaler. A job on a branch other than `main` runs, but its OIDC step is refused if the policy accepts only `main`.

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
    runs-on: linux-arm64-deploy   # scaling.label
    steps:
      - uses: actions/checkout@v4
      # ... aws-actions/configure-aws-credentials with role-to-assume ...
      - run: docker buildx build --push -t <registry>/<image>:<tag> .
```

- The repository must be allowed in the runner group of the organization (see [GitHub setup](#github-setup)). Nothing else is registered per repository.
- `runs-on` is the `scaling.label` value. It is the same in every organization.
- Jobs started by a fork pull request are refused (see [Job gate](#job-gate)).
- Jobs that use `environment:` are not supported, because the OIDC subject changes.
- Build and push with `docker buildx build --push`. The runner has no Docker daemon, and its default builder already points at the runner's BuildKit.
- The runner has the aws CLI v2, kubectl, git, the docker CLI, and buildx (arm64).

## Troubleshooting

- **Start with `status`.** In error-wait (`상태: 오류 대기`), the `오류:` line holds every config error or the permanent error that stopped startup (missing image, Docker TCP 2375, `server runner limit is 0`, authentication). The scaler makes no GitHub or Docker calls in this state. Fix the cause, then restart with `service stop` and `launchctl kickstart` (see Restart). A target in `오류 대기` shows its own error on its line and retries without a restart (see [Target errors](#target-errors)).
- **Read the log:** `~/Library/Logs/actions-runner-scaler/stderr.log`, one JSON object per line, for example `tail -f … | jq .`.
- **`실행 중 아님`:** the agent is not loaded or the process exited. Check the log and `launchctl print gui/$(id -u)/io.github.dewevrei.actions-runner-scaler`.
- **A non-zero `연속 시작 실패`:** runner starts are failing, for example because an image was removed, a CPU or memory value is too large for Docker, or JIT issuance failed. After 3 failures in a row, starts back off from 10 s up to 5 min.
- **A job sits at "Set up job" and then fails:** the gate denied it or never allowed it. Search the log for the runner name to see the reason.
- **A job waits for a long time:** `status` shows `대기 job` above 0 when the server limit or the target's `max-runners` leaves no free slot. The job starts when a slot frees up. Raise the limit as described in [Server limit](#server-limit) if this is common.
- **`message poll got no answer` (WARN):** GitHub's service accepted the connection but did not answer a poll for more than 70 s, so the scaler opens a new session. If it repeats, check the network.
- **`message poll rate limited` (WARN):** GitHub limited the polls. The log has `status` and `retryAt`, the time the poll can be repeated.
- **`job no longer available` (INFO):** a job the scaler wanted to take was already taken elsewhere or cancelled. It is dropped from the waiting list. No action.
- **`waiting job dropped` (INFO):** the scaler removed a waiting-list entry that GitHub no longer counts as an assigned job. If GitHub offers or assigns that job again, it is added again.
- **`GitHub counts more available jobs than the scaler knows` (WARN):** GitHub reports more unassigned jobs than the scaler has on its waiting list, so a job message may have been missed. The scaler only logs it and changes nothing.
- **Leftover containers after a crash:** they remain until the next start. At start the scaler removes every container and network it labeled, and the stale registrations of each target's scale set.
