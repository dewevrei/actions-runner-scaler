# actions-runner-scaler

Ephemeral GitHub Actions self-hosted runners in Docker on one Mac, scaled with the jobs of chosen repositories.

One Go program for a team that wants its build and deploy jobs to run on one macOS host (Apple Silicon) instead of on GitHub-hosted runners. For each configured repository it registers a runner scale set with GitHub's [`actions/scaleset`](https://github.com/actions/scaleset) library (v0.5.0), starts one runner container per job, and removes it when the job ends.

> **Status:** under development. The scaler was run against real GitHub on 2026-11-26 with a GitHub App (registration, concurrency, waiting jobs, the LaunchAgent, target errors, `delete-scaleset`). Not yet verified in real use: token targets in the scaler, `doctor`, `init`, `add-repo`, `remove-repo`, `app-link`, a host reboot, and the fork pull request refusal (unit tests only).

## Contents

[How it works](#how-it-works) · [Requirements](#requirements) · [Quick start](#quick-start) · [GitHub setup](#github-setup) · [Building](#building) · [Workflow requirements](#workflow-requirements) · [Configuration](#configuration) · [Commands](#commands) · [Running as a service](#running-as-a-service) · [Security model](#security-model) · [Updating the runner version](#updating-the-runner-version) · [Troubleshooting](#troubleshooting) · [Development](#development) · [License](#license)

## How it works

- One **target** is one repository (`[[targets]]` in the config), of an organization or a personal account. Each gets its own repository-level scale set named `scaling.label` (default `linux-arm64-deploy`), in runner group ID 1. A job with `runs-on: linux-arm64-deploy` reaches only its own repository's scale set.
- Each target keeps about as many runners as it has assigned jobs plus `idle`, never more than its `max-runners` (default 1). All targets share one **server limit** computed from Docker's CPUs and memory; jobs that do not fit wait in GitHub's queue.
- Each runner runs one job, in its own runner and BuildKit containers (see [Security model](#security-model)). When the job ends, the scaler removes them and the runner's GitHub registration.
- A launchd LaunchAgent starts the scaler at login and restarts it after a crash.

## Requirements

- macOS on Apple Silicon. Images are `linux/arm64` only.
- Docker Desktop with **Settings > Advanced > Allow the default Docker socket to be used** on. The scaler uses `/var/run/docker.sock` and ignores `DOCKER_HOST`.
- Docker must not answer on `127.0.0.1:2375` (see [Security model](#security-model)).
- Go 1.27.3 or later, to build.
- A credential per repository: a GitHub App or a personal access token (see [GitHub setup](#github-setup)).

## Quick start

```sh
go build -o bin/actions-runner-scaler ./cmd/actions-runner-scaler

# 1. Print the App registration link (-org <organization> for an organization's App).
bin/actions-runner-scaler app-link

# 2. Open the link, create the App, generate a private key, install the App on your repositories.
mkdir -p -m 700 ~/.config/actions-runner-scaler
mv ~/Downloads/<app-key>.pem ~/.config/actions-runner-scaler/
chmod 600 ~/.config/actions-runner-scaler/<app-key>.pem

# 3. Build the runner image and pull the BuildKit image.
image/build.sh

# 4. Write the config (fills runner.image and buildkit.image from step 3).
bin/actions-runner-scaler init \
  -app-client-id <client-id> \
  -app-private-key-file "$HOME/.config/actions-runner-scaler/<app-key>.pem" \
  -repo https://github.com/<owner>/<repo>

# 5. Check config, credential, GitHub and Docker. Exit 0 means no check failed.
bin/actions-runner-scaler doctor

# 6. Run in the foreground, or install the LaunchAgent instead.
bin/actions-runner-scaler run
# bin/actions-runner-scaler service install
```

With a token instead of an App, skip steps 1–2 and run `init -token-file <absolute path> -repo <URL>`. Without flags, `init` asks for missing values on the terminal. Then use `runs-on: linux-arm64-deploy` in a workflow of that repository.

## GitHub setup

The owner of each repository provides the credential (and installs the App). A target that is not set up stops with an error in `status`; the other targets keep running.

| Credential | Needs | Notes |
|---|---|---|
| GitHub App (preferred) | Repository permission **Administration: Read and write**; installed with the repository in its selection | not tied to a person; rate limit per installation |
| Fine-grained token | Resource owner = the repository's owner, **Only select repositories** with that repository, **Administration: Read and write** | one owner only; Read-only passes start-up but fails at scale set creation |
| Classic token (last resort) | `repo` scope and admin access to the repository | reaches every repository the person can reach |

- **App.** `app-link` prints a link that preselects the permission, turns webhooks off and makes the App private (`-public` to let other accounts install it). Store the private key outside the repository with mode `0600` or `0400`. There is no installation ID to configure; the scaler looks it up per repository.
- **Tokens.** Put the token alone in a file (one line, same mode as the key, absolute path), and put the file's path in the config, never the token: `github.token-file` as the default, or `token-file` in one `[[targets]]` entry. Tip: `install -m 600 /dev/null <path>`, then paste with an editor. `doctor` warns when a token expires within 7 days.
- **Several owners.** The config has one default credential (one App at most). Serve another owner's repositories with a per-target `token-file` or a public App. A personal account's repository needs a token made by its owner.
- **Public repositories.** GitHub advises against self-hosted runners for them. The job gate does not stop everything (see [Security model](#security-model)); also require approval for fork workflow runs in GitHub's settings.
- **Adding a repository:** give it a credential, `add-repo`, `doctor`, restart.
- **Removing a repository** (scale sets are never deleted on stop or config change): `service stop` → `delete-scaleset <URL>` while it is still in the config → `remove-repo <URL>` → restart. For a rename or move, delete with the old URL first, then change the URL.

## Building

```sh
go build -ldflags "-X main.version=v0.1.0" -o bin/actions-runner-scaler ./cmd/actions-runner-scaler
image/build.sh
```

`image/build.sh` pulls the BuildKit image pinned at its top and builds `actions-runner-scaler-runner:local` from `image/Dockerfile` (official `actions-runner` base plus aws CLI v2, kubectl, docker CLI and buildx). It checks both are arm64 and prints the runner image ID and the pinned BuildKit reference:

```
# scaler config values
runner.image = "sha256:<runner-image-id>"
buildkit.image = "moby/buildkit:<version>-rootless@sha256:<index-digest>"
```

- Each key takes an image reference (`<name>[:<tag>][@<digest>]`) or a local image ID. The scaler pulls a reference that is missing locally, without registry credentials, at start and when a runner creation finds it missing; pull an image from a private registry beforehand with `docker pull`. An ID cannot be pulled, so do not prune the image an ID names.
- `init` writes `runner.image = "actions-runner-scaler-runner:local"`, the tag of the image `image/build.sh` builds, and the printed `buildkit.image`. With that tag, rerunning `image/build.sh` needs no config edit and no restart: runners created afterwards use the new image. With an image ID, put the new ID in the config and restart the scaler or the service.
- `init` fills `runner.image`, and `doctor` passes it, only when the image has the labels `image/build.sh` sets (the scaler itself runs without them). If `doctor` shows `확인 못 함` for a local `runner.image` because of the labels, rerun the script.
- To change the BuildKit version, set `BUILDKIT_IMAGE` in `image/build.sh` and `BuildKitImage` in `internal/docker/image.go` to the same value (a test checks this), rebuild, rerun `image/build.sh`, and update `buildkit.image`.

## Workflow requirements

```yaml
permissions:
  id-token: write      # AWS credentials via GitHub OIDC
  contents: read
jobs:
  deploy:
    runs-on: linux-arm64-deploy   # scaling.label
    steps:
      - uses: actions/checkout@v4
      # ... aws-actions/configure-aws-credentials with role-to-assume ...
      - run: docker buildx build --push -t <registry>/<image>:<tag> .
```

- The repository must be in `[[targets]]` with a working credential.
- Build with `docker buildx build --push`: there is no Docker daemon, and the default builder already uses the runner's BuildKit. The build cache starts empty for each job.
- Jobs that use `environment:` are not supported (the OIDC subject changes).
- The runner has the aws CLI v2, kubectl, git, the docker CLI and buildx.

## Configuration

Default path: `~/.config/actions-runner-scaler/config.toml` (every command takes `-config <path>`). Write it with `init`, or start from [`config.example.toml`](config.example.toml), which lists every key with its default. Secrets are set by file path only. The scaler reads the config and token files once at start, so restart after any change (see [Running as a service](#running-as-a-service)). A config error puts the scaler in error-wait with the problems listed in `status`.

| Key | Default | Meaning |
|---|---|---|
| `github.app-client-id`, `github.app-private-key-file` | none | default credential: the App |
| `github.token-file` | none | default credential: a token file (not with the App keys) |
| `runner.image`, `buildkit.image` | required | image reference or local image ID (see [Building](#building)) |
| `runner.cpus` / `memory` / `pids` | `1` / `1g` / `512` | limits per runner container |
| `buildkit.cpus` / `memory` / `pids` | `2` / `2g` / `1024` | limits per BuildKit container |
| `scaling.label` | `linux-arm64-deploy` | scale set name and `runs-on` label, same for every target |
| `jobcheck.listen` | `127.0.0.1:8765` | job gate address (loopback only) |
| `shutdown.drain-timeout` | `10m` | how long a stop waits for busy jobs |
| `targets.url` | required | `https://github.com/<owner>/<repo>`, one entry per repository |
| `targets.max-runners` | `1` | most runners of this target at once |
| `targets.idle` | `0` | runners kept waiting for a job |
| `targets.token-file` | none | token file for this target instead of the default credential |

**Server limit.** The scaler never runs more runners in total than `min(floor(CPUs / (runner.cpus + buildkit.cpus)), floor(memory / (runner.memory + buildkit.memory)))`, using Docker's CPUs and memory at start. With Docker at 10 CPUs and 8 GiB and the defaults, it is 2. Raise it by giving Docker more resources or lowering the per-runner limits, then restart. A limit of 0, or a sum of `idle` above it, is an error; a sum of `max-runners` above it is allowed.

Older configs (organization URLs, `runner-group`, `[[pools]]`, `app-installation-id`) are rejected with an error that says what to change.

## Commands

| Command | What it does |
|---|---|
| `run [-config <path>]` | runs the scaler in the foreground (JSON log on stderr) |
| `status` | prints the running scaler's state |
| `init [flags]` | writes a new config file (never overwrites) |
| `add-repo [flags] <URL>` / `remove-repo <URL>` | adds or removes one `[[targets]]` block, keeping the rest of the file |
| `doctor [-config <path>]` | checks config, credentials, GitHub, Docker, images and the server limit |
| `app-link [-org <organization>] [-public]` | prints the link that creates the GitHub App |
| `delete-scaleset [-config <path>] <URL>` | deletes a configured repository's scale set (scaler must be stopped) |
| `service install\|stop\|uninstall` | manages the LaunchAgent |
| `version` | prints the version |

Run `bin/actions-runner-scaler help` or `<command> -h` for flags. `doctor`, `status`, `init`, `add-repo`, `remove-repo` and `app-link` are safe while the scaler runs.

**Signals (`run`).** The first SIGINT or SIGTERM starts a drain: no new runners, idle runners removed, busy jobs finish (up to `drain-timeout`), then exit 0. A second signal removes what is left and exits at once.

**`status`** (output in Korean). First line: `상태: 시작 중`, `상태: 정상`, `상태: 정상 target 없음`, `상태: drain 중`, `상태: 오류 대기` (whole scaler stopped until restart), or `실행 중 아님`. Then, unless in error-wait, one line per target:

```
상태: 정상
https://github.com/<owner>/<repo>: 정상, 실행 중 1, 대기 0, 대기 job 0, 최대 1, 연속 시작 실패 0
전체: 실행 중 1, 대기 0, 최대 2
마지막 오류: 없음
```

A target with a GitHub setup problem shows `오류 대기` with its error and retries on its own (from 30 s up to every 30 minutes) without a restart.

**`doctor`** prints one line per check as `통과`, `실패` or `확인 못 함`, with a cause and fix for each problem. It exits 1 only when a check is `실패`. It only reads (for an image reference missing locally it asks the registry through Docker and pulls nothing), and never prints key or token values.

## Running as a service

```sh
bin/actions-runner-scaler service install     # write the LaunchAgent plist and start; again = graceful restart
bin/actions-runner-scaler service stop        # drain and stop (waits up to drain-timeout + 90 s)
bin/actions-runner-scaler service uninstall   # stop, unload and remove the plist
launchctl kickstart gui/$(id -u)/io.github.dewevrei.actions-runner-scaler   # start again after stop
```

- The plist records the binary's absolute path, so keep the binary there (a `go run` binary is refused).
- launchd restarts the scaler after a crash (at most once every 60 s), not after a clean stop.
- Logs: `~/Library/Logs/actions-runner-scaler/stderr.log` (JSON) and `stdout.log`.
- **Restart** after editing the config or a token file, or fixing an error-wait: `service stop`, then `launchctl kickstart` (or `service install` again).
- **Do not use `launchctl bootout` or `kickstart -k` to stop it.** Since macOS 26.4 launchd kills the process 60 s after SIGTERM, which cuts a drain short and loses the running job. Run `service stop` before you restart or shut down the Mac.

## Security model

- **Containers.** The runner container is non-root, with all capabilities dropped, `no-new-privileges`, CPU/memory/PID limits, no host mounts and no Docker socket. The rootless BuildKit container adds back only `SETUID` and `SETGID` and runs with `seccomp=unconfined`. Both share only a private network and are removed after the job.
- **No credentials in containers.** The scaler passes no AWS, cluster or GitHub credentials into containers. Jobs get AWS credentials through GitHub OIDC, and the AWS trust policy decides which job gets a role.
- **Secrets by path only.** A pasted token in place of a path, or a URL with a credential in it, is refused without printing it. Logs and command output never show token values.
- **Job gate.** Before any step runs, a job-started hook reports the job's event to the scaler, which refuses jobs that come from a fork pull request, by comparing repository IDs in the event file. Everything else runs, including any branch and same-repository pull requests; who started a job is left to GitHub's settings and the AWS trust policy.
- **Docker TCP 2375.** If Docker answers on `127.0.0.1:2375`, any container could control the host's Docker, so the scaler refuses to start. Turn off **Expose daemon on tcp://localhost:2375 without TLS** and stop anything listening there (`lsof -nP -iTCP:2375 -sTCP:LISTEN`).

## Updating the runner version

Runner self-update is off, so the image fixes the version. GitHub stops assigning jobs to a runner version 63 days after a newer one is available. Check the date:

```sh
gh api repos/<owner>/<repo>/actions/runners/deprecations/<version> --jq .runtime_deprecates_at
```

Before then: change the `FROM ghcr.io/actions/actions-runner:<version>@sha256:<digest>` line in `image/Dockerfile` (digest from `docker buildx imagetools inspect`), recheck `image/job-started.sh` against that version, run `image/build.sh` and apply the new image as in [Building](#building).

## Troubleshooting

Start with `doctor` and `status`, then the log (`tail -f ~/Library/Logs/actions-runner-scaler/stderr.log | jq .`).

| Symptom | What to do |
|---|---|
| `상태: 오류 대기` | the `오류:` line names the cause (config, missing image, Docker TCP 2375, server limit). Fix it and restart |
| target `오류 대기` with `GitHub App is not installed on repository <owner>/<repo>` | install the App on the owner, with that repository selected. Retries on its own |
| target error `... lacks administration: write` | grant (or approve) Administration: Read and write. Retries on its own |
| target error `personal access token ... was refused (401)` | token invalid, expired or revoked: replace the file's content and restart |
| target error `personal access token ... cannot manage its runners` | check the token against [GitHub setup](#github-setup); the text after `;` says what GitHub expects |
| `value looks like a token, not a file path` or `value contains a credential` | put the token in a file and set its path; make a new token if it may have been seen |
| `실행 중 아님` | the agent is not loaded or exited: check the log and `launchctl print gui/$(id -u)/io.github.dewevrei.actions-runner-scaler` |
| non-zero `연속 시작 실패` | runner starts fail (image removed, limits too large for Docker, JIT failure); starts back off up to 5 min |
| job stops at "Set up job" and fails | the job gate denied it, or got no report within 180 s; search the log for the runner name |
| job waits long (`대기 job` above 0) | no free slot under `max-runners` or the server limit |
| leftover containers after a crash | removed at the next start, with stale registrations |

## Development

```sh
go build ./...
go test -race ./...
```

- Tests with the `integration` build tag call real GitHub and Docker; delete what they create.
- `image/build.sh --record` builds a diagnostic runner image; see the comment at the top of the script.
- Design documents for each change are in `intent/NNNN-<name>/` (`INTENT.md`, `SPEC.md`, `PLAN.md`); conventions are in `AGENTS.md`.

## License

MIT License. See [LICENSE](LICENSE).
