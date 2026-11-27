# actions-runner-scaler

Ephemeral GitHub Actions self-hosted runners in Docker on one Mac, scaled with the jobs of chosen repositories.

One Go program for a team that wants its build and deploy jobs to run on one macOS host (Apple Silicon) instead of on GitHub-hosted runners. For each configured repository it registers a runner scale set with GitHub's [`actions/scaleset`](https://github.com/actions/scaleset) library (v0.5.0), starts one runner container per job, and removes it when the job ends.

> **Status:** under development. Run against real GitHub on 2026-11-26 and 2026-11-27: GitHub App and token targets (classic and fine-grained), concurrency, waiting jobs under a server limit of 1 across two repositories, the LaunchAgent and `service start`, target errors, Docker start-up retries, `delete-scaleset`, `init`, `add-repo`, `doctor`, image rebuilds and pulls. Not yet verified in real use: acquiring a `JobAvailable` job (GitHub assigned every job directly), fixing `DisableUpdate` on an existing scale set, targets on different GitHub Apps, `remove-repo`, the page `app-link` opens, a host reboot, and the fork pull request refusal (unit tests only).

## Contents

[How it works](#how-it-works) · [Requirements](#requirements) · [Quick start](#quick-start) · [GitHub setup](#github-setup) · [Building](#building) · [Workflow requirements](#workflow-requirements) · [Configuration](#configuration) · [Commands](#commands) · [Running as a service](#running-as-a-service) · [Security model](#security-model) · [Updating the runner version](#updating-the-runner-version) · [Troubleshooting](#troubleshooting) · [Development](#development) · [License](#license)

## How it works

- One **target** is one repository (`[[targets]]` in the config), of an organization or a personal account, so each repository keeps its own maximum. Each gets its own repository-level scale set named `scaling.label` (default `linux-arm64-deploy`, with no other labels), in runner group ID 1. A job with `runs-on: linux-arm64-deploy` reaches only its own repository's scale set.
- Each target keeps as many runners as it has assigned jobs plus `idle`, never more than its `max-runners` (default 1); the scaler acquires every offered job at once. All targets share one **server limit**, computed from Docker's CPUs and memory unless set in the config. Runner starts pass one server-wide queue in the order they were asked for, starts for assigned jobs before starts for `idle`; a job that does not fit stays assigned to its scale set at GitHub until a slot frees, and an idle runner of another target is removed when it holds the slot that job needs.
- Each runner runs one job, in its own runner container on its own network, with a BuildKit container beside it for image builds only when `buildkit.enabled = true` (see [Security model](#security-model)). When the job ends, the scaler removes them and the runner's GitHub registration.
- A launchd LaunchAgent starts the scaler at login and restarts it after a crash; an unattended reboot therefore needs macOS automatic login.

## Requirements

- macOS on Apple Silicon. Images are `linux/arm64` only.
- Docker Desktop. The scaler connects to `docker.host` (default `unix:///var/run/docker.sock`, which needs **Settings > Advanced > Allow the default Docker socket to be used**) and ignores `DOCKER_HOST`.
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

# 3. Build the runner image (it also pulls the BuildKit image, used only with buildkit.enabled = true).
image/build.sh

# 4. Write the config (fills runner.image from step 3 and buildkit.image with the pinned reference).
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

- **App.** `app-link` prints a link that preselects the permission, turns webhooks off and makes the App private (`-public` to let other accounts install it). Store the private key outside the repository, readable by you only (no group or other permission bits, such as `0600` or `0400`). There is no installation ID to configure; the scaler looks it up per repository.
- **Tokens.** Put the token alone in a file (one line, same mode as the key, absolute path), and put the file's path in the config, never the token: `github.token-file` as the default, or `token-file` in one `[[targets]]` entry. Tip: `install -m 600 /dev/null <path>`, then paste with an editor. `doctor` warns when a token expires within 7 days.
- **Several owners.** The config has one default credential, in `[github]`; a target with a credential of its own (`token-file`, or `app-client-id` with `app-private-key-file`) uses that instead, and `[github]` may be left out when every target has one. Serve another owner's repositories with their own App or token, or with one public App. A personal account's repository needs an App or a token of its owner.
- **Public repositories.** GitHub advises against self-hosted runners for them. The job gate does not stop everything (see [Security model](#security-model)); also require approval for fork workflow runs in GitHub's settings.
- **Adding a repository:** give it a credential, `add-repo` (with `-token-file`, or `-app-client-id` and `-app-private-key-file`, when it has its own), `doctor`, restart.
- **Removing a repository** (scale sets are never deleted on stop or config change): `service stop` → `delete-scaleset <URL>` while it is still in the config → `remove-repo <URL>` → `service start`. For a rename or move, delete with the old URL first, then change the URL.

## Building

```sh
go build -ldflags "-X main.version=v0.1.0" -o bin/actions-runner-scaler ./cmd/actions-runner-scaler
image/build.sh
```

`image/build.sh` pulls the BuildKit image pinned at its top and builds `actions-runner-scaler-runner:local` from `image/Dockerfile` (official `actions-runner` base plus aws CLI v2, kubectl, docker CLI and buildx). It checks both are arm64 and prints the values for the config:

```
# scaler config values
runner.image = "actions-runner-scaler-runner:local"
# runner image ID: sha256:<runner-image-id>
buildkit.image = "moby/buildkit:<version>-rootless@sha256:<index-digest>"
```

- Each key takes an image reference (`<name>[:<tag>][@<digest>]`) or a local image ID. The scaler pulls a reference that is missing locally, without registry credentials, at start and when a runner creation finds it missing; pull an image from a private registry beforehand with `docker pull`. A tag already present locally is never refreshed; run `docker pull` to update it. Keep the image an ID names; it cannot be pulled.
- `init` fills `runner.image` with the tag when that image is present locally, and `buildkit.image` with the pinned reference without looking it up, and writes `buildkit.enabled = false`. With the tag, rerunning `image/build.sh` needs no config edit and no restart: runners created afterwards use the new image. With an image ID, put the new ID in the config and restart the scaler or the service.
- The BuildKit image is used only with `buildkit.enabled = true`; with it false (the default), the scaler and `doctor` never look it up or pull it, though `buildkit.image` must still hold a valid value.
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
      - run: docker buildx build --push -t <registry>/<image>:<tag> .   # needs buildkit.enabled = true
```

- The repository must be in `[[targets]]` with a working credential.
- **Building images on this host needs `buildkit.enabled = true`** in the config (then restart). With the default `false`, runners have no BuildKit and no Docker daemon, so `docker build` and `docker buildx build` fail; such jobs belong on GitHub-hosted runners. Jobs that only run tools such as kubectl or crane need nothing more.
- With BuildKit enabled, build with `docker buildx build --push`: there is no Docker daemon, and the default builder already uses the runner's BuildKit. The build cache starts empty for each job.
- Jobs that use `environment:` are not supported (the OIDC subject changes).
- The runner has the aws CLI v2, kubectl, git, the docker CLI and buildx.

## Configuration

Default path: `~/.config/actions-runner-scaler/config.toml` (every command takes `-config <path>`). Write it with `init`, or start from [`config.example.toml`](config.example.toml), which lists every key with its default. Secrets are set by file path only. The scaler reads the config and token files once at start, so restart after any change (see [Running as a service](#running-as-a-service)). A config error puts the scaler in error-wait with the problems listed in `status`.

| Key | Default | Meaning |
|---|---|---|
| `github.app-client-id`, `github.app-private-key-file` | none | default credential, for targets without their own: the App |
| `github.token-file` | none | default credential: a token file (not with the App keys) |
| `runner.image`, `buildkit.image` | required | image reference or local image ID (see [Building](#building)) |
| `runner.cpus` / `memory` / `pids` | `1` / `1g` / `512` | limits per runner container |
| `buildkit.enabled` | `false` | `true` starts a BuildKit container beside each runner and sets `BUILDKIT_HOST`, for `docker buildx build` in jobs |
| `buildkit.cpus` / `memory` / `pids` | `2` / `2g` / `1024` | limits per BuildKit container (used only when `buildkit.enabled` is true) |
| `scaling.label` | `linux-arm64-deploy` | scale set name and `runs-on` label, same for every target |
| `scaling.server-limit` | `0` | most runners of all targets at once; `0` computes it (below) |
| `docker.host` | `unix:///var/run/docker.sock` | Docker endpoint, `unix://<path>` or `tcp://<host>:<port>` |
| `log.level` | `info` | `debug`, `info`, `warn` or `error` |
| `jobcheck.listen` | `127.0.0.1:8765` | job gate address (loopback only) |
| `shutdown.drain-timeout` | `10m` | how long a stop waits for busy jobs |
| `targets.url` | required | `https://github.com/<owner>/<repo>`, one entry per repository |
| `targets.max-runners` | `1` | most runners of this target at once |
| `targets.idle` | `0` | runners kept waiting for a job (ARC's `minRunners`) |
| `targets.token-file` | none | token file for this target instead of the default credential |
| `targets.app-client-id`, `targets.app-private-key-file` | none | GitHub App for this target instead of the default credential (not with `token-file`) |

**Server limit.** With `scaling.server-limit = 0` the scaler never runs more runners in total than `min(floor(CPUs / runner.cpus), floor(memory / runner.memory))`, using Docker's CPUs and memory at start; with `buildkit.enabled = true` each runner also counts its BuildKit container: `min(floor(CPUs / (runner.cpus + buildkit.cpus)), floor(memory / (runner.memory + buildkit.memory)))`. With Docker Desktop at 10 CPUs and 8 GiB (it reports a little less memory) and the defaults, it is 7, or 2 with BuildKit enabled. Raise it by giving Docker more resources or lowering the per-runner limits, or set `scaling.server-limit` to the number you want, then restart. A limit of 0, or a sum of `idle` above it, is an error; a sum of `max-runners` above it is allowed.

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
| `service install\|start\|stop\|restart\|uninstall` | manages the LaunchAgent |
| `version` | prints the version |

Run `bin/actions-runner-scaler help` or `<command> -h` for flags. `doctor`, `status`, `init`, `add-repo`, `remove-repo` and `app-link` are safe while the scaler runs.

**Signals (`run`).** The first SIGINT or SIGTERM starts a drain: no new runners, idle runners removed, busy jobs finish (up to `drain-timeout`), then exit 0. A second signal removes what is left and exits at once.

**`status`**. First line: `state: starting`, `state: running`, `state: running, no target`, `state: draining`, `state: error-wait` (followed only by an `error:` line), or `not running`. Then one line per target, a `total` line (not while starting) and the last error:

```
state: running
https://github.com/<owner>/<repo>: running, busy 1, idle 0, waiting jobs 0, max 1, start failures 0
total: busy 1, idle 0, max 2
last error: none
```

`waiting jobs` counts the target's assigned jobs that no runner covers yet. A target with a GitHub setup problem shows `error-wait`, its error and `next retry <time>`, and retries on its own (from 30 s up to every 30 minutes) without a restart.

**`doctor`** prints one line per check as `PASS`, `FAIL` or `UNKNOWN`, with `cause:` and `fix:` lines for each problem, and a `PASS <n>, FAIL <n>, UNKNOWN <n>` total. It exits 1 only when a check is `FAIL`. It only reads (for an image reference missing locally it asks the registry through Docker and pulls nothing), and never prints key or token values. Its GitHub checks are REST permission checks; they do not reach the Actions service the scaler talks to. Its `Server limit` check uses the same limit as the scaler: `scaling.server-limit` when set, otherwise the value computed from Docker for the current `buildkit.enabled`. With `buildkit.enabled` false, the `Image buildkit.image` line passes as `not used, buildkit.enabled is false` without a lookup.

## Running as a service

```sh
bin/actions-runner-scaler service install     # write the LaunchAgent plist and start; again = graceful restart
bin/actions-runner-scaler service start       # start the installed agent's scaler, wait until it answers
bin/actions-runner-scaler service stop        # drain and stop (waits up to drain-timeout + 90 s)
bin/actions-runner-scaler service restart     # stop, then start
bin/actions-runner-scaler service uninstall   # stop, unload and remove the plist
```

- The plist records the binary's absolute path, so keep the binary there (a `go run` binary is refused).
- launchd restarts the scaler after a crash (at most once every 10 s), not after a clean stop.
- Logs: `~/Library/Logs/actions-runner-scaler/stderr.log` (JSON) and `stdout.log`.
- **Restart** with `service restart` after editing the config or a token file, or fixing a config error.
- **Do not use `launchctl bootout` or `kickstart -k` to stop it.** Since macOS 26.4 launchd kills the process 60 s after SIGTERM, which cuts a drain short and loses the running job. Run `service stop` before you restart or shut down the Mac.

## Security model

- **Containers.** The runner container is non-root without `sudo` (steps that use `sudo` or `apt-get` fail), with all capabilities dropped, `no-new-privileges`, CPU/memory/PID limits, no host mounts and no Docker socket, on a private network of its own, and is removed after the job. With `buildkit.enabled = true`, a rootless BuildKit container joins that network; it adds back only `SETUID` and `SETGID`, runs with `seccomp=unconfined`, and is removed with the runner. BuildKit containers left by an earlier run are removed whatever the setting.
- **No credentials in containers.** The scaler passes no AWS, cluster or GitHub credentials into containers. Jobs get AWS credentials through GitHub OIDC, and the AWS trust policy decides which job gets a role.
- **Secrets by path only.** A pasted token in place of a path, or a URL with a credential in it, is refused without printing it. Logs and command output never show token values.
- **Job gate.** Before any step runs, a job-started hook reports the job's event to the scaler, which refuses jobs that come from a fork pull request, by comparing repository IDs in the event file. Everything else runs, including any branch and same-repository pull requests; who started a job is left to GitHub's settings and the AWS trust policy.
- **Docker TCP 2375.** If Docker answers on `127.0.0.1:2375`, any container could control the host's Docker, so the scaler starts no runner and stays in error-wait, checking again, until the port is closed; a `docker.host` of `tcp://127.0.0.1:2375` (or `localhost`, `::1`) is refused at start as a config error. Turn off **Expose daemon on tcp://localhost:2375 without TLS** and stop anything listening there (`lsof -nP -iTCP:2375 -sTCP:LISTEN`).

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
| `state: error-wait` | the `error:` line names the cause. A config error needs a fix and a restart; Docker-side causes (Docker not answering, a missing image, Docker TCP 2375, a computed server limit of 0 or below the sum of `idle`) are retried on their own, and the error shows the next retry time |
| target `error-wait` with `GitHub App is not installed on repository <owner>/<repo>` | install the App on the owner, with that repository selected. Retries on its own |
| target error `... lacks administration: write` | grant (or approve) Administration: Read and write. Retries on its own |
| target error `personal access token ... was refused (401)` | token invalid, expired or revoked: replace the file's content and restart |
| target error `personal access token ... cannot manage its runners` | check the token against [GitHub setup](#github-setup); the text after `;` says what GitHub expects |
| `value looks like a token, not a file path` or `value contains a credential` | put the token in a file and set its path; make a new token if it may have been seen |
| `not running` | the agent is not loaded or exited: check the log and `launchctl print gui/$(id -u)/io.github.dewevrei.actions-runner-scaler` |
| non-zero `start failures` | runner starts fail (image removed, limits too large for Docker, JIT failure); starts back off up to 5 min |
| job stops at "Set up job" and fails | the job gate denied it, or got no report within 180 s; search the log for the runner name |
| job waits long (`waiting jobs` above 0) | no free slot under `max-runners` or the server limit |
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
