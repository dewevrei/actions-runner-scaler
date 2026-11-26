# actions-runner-scaler

Ephemeral GitHub Actions self-hosted runners in Docker on one Mac, scaled with the jobs of chosen repositories.

actions-runner-scaler is one Go program for a team that wants its own build and deploy jobs to run on one macOS host (Apple Silicon) instead of on GitHub-hosted runners. For each GitHub repository listed in its config, it registers a runner scale set with GitHub's [`actions/scaleset`](https://github.com/actions/scaleset) library (v0.5.0). When jobs are assigned to that scale set, it starts one runner container per job, each with its own rootless BuildKit for `docker buildx build`, and removes everything when the job ends. It authenticates to GitHub with a GitHub App or with personal access tokens, and it includes commands that write, edit and check its config.

> **Status:** under development. Verified so far:
>
> - An earlier, organization-level version was run against real GitHub on 2026-11-26: registration, jobs of the same repository (including `pull_request`, `pull_request_target` and `workflow_run` events), concurrency, waiting for a free slot, a restart while a job waits, the LaunchAgent service, an unresponsive service, and the log checks.
> - The repository-level design was checked against real GitHub on 2026-11-26: two repositories of one owner each held a scale set with the same name in runner group ID 1, a repository of a personal account accepted runner group ID 1, and the App's installation lookup answered 404 for repositories outside an installation's repository selection.
> - The repository-level scaler was run against real GitHub on 2026-11-26 with the GitHub App: runners listed only in their own repository, stale registrations removed after `kill -9`, a personal-account repository, per-repository `max-runners` under the server limit, waiting jobs taking freed slots, smaller per-runner resources, the LaunchAgent (install, stop, restart, uninstall), a target without the App installed going to `오류 대기` while the others kept running, and `delete-scaleset`.
> - On 2026-11-26 a separate check program using the same library registered a repository scale set, opened a message session, issued and removed a runner, and deleted the scale set, once with a classic token (`repo` scope) and once with a fine-grained token (one repository, Administration: Read and write).
>
> Not verified yet: the scaler with token targets and `doctor` against real GitHub, the setup commands `init`, `add-repo`, `remove-repo` and `app-link` in real use, whether the registration page that `app-link` opens shows its values selected (these real runs are pending), a reboot of the host, a repository removed from and added back to an App installation while the scaler runs, jobs that wait longer than GitHub's assignment retries, and the refusal of fork pull requests against real GitHub (the refusal rules are covered by unit tests only). Commands below describe the intended behavior and match the code's command-line interface.

## Contents

- [How it works](#how-it-works)
- [Requirements](#requirements)
- [Quick start](#quick-start)
- [GitHub setup](#github-setup)
- [Building](#building)
- [Workflow requirements](#workflow-requirements)
- [Configuration](#configuration)
- [Commands](#commands)
- [Running as a service](#running-as-a-service)
- [Security model](#security-model)
- [Updating the runner version](#updating-the-runner-version)
- [Troubleshooting](#troubleshooting)
- [Development](#development)
- [License](#license)

## How it works

- One **target** is one GitHub repository (`[[targets]]` in the config), owned by an organization or a personal account. For each target the scaler registers its own runner scale set at the repository level, in runner group ID 1 (the default runner group), with the name `scaling.label` (`linux-arm64-deploy` by default). A job in that repository with `runs-on:` set to the `scaling.label` value (default `linux-arm64-deploy`) reaches that repository's scale set only. A scale set name is unique per repository, so every target can use the same name. A repository that is not in `[[targets]]` gets no runners from the scaler.
- Each target authenticates with a **credential**: the GitHub App, or a personal access token read from a file. The config has one default credential, and a target can name its own token file (see [GitHub setup](#github-setup)). A target that uses the App is an **App target**, one that uses a token a **token target**.
- Each target keeps about as many runners as it has assigned jobs plus its `idle` setting, never more than its own `max-runners` (default 1). Each runner is one job: when the job ends the container exits, and the scaler removes its resources and its GitHub registration.
- All targets share one **server limit** on the number of runners (see [Server limit](#server-limit)). Jobs that do not fit wait in GitHub's queue and are meant to get a slot as one frees up, the one GitHub queued first going first, whichever repository it belongs to (not yet verified for jobs that wait longer than GitHub's assignment retries).
- Each runner gets its own resource set, created and removed together:
  - a runner container: non-root, all capabilities dropped, `no-new-privileges`, CPU/memory/PID limits, no host mounts, no Docker socket
  - a rootless BuildKit container: CPU/memory/PID limits, `seccomp=unconfined`, only `SETUID` and `SETGID` added back
  - a private bridge network that only those two containers join
- `docker buildx build` inside a job uses that BuildKit through a preconfigured default builder. The build cache starts empty and is deleted with the runner.
- A **job gate** blocks the first step of each job until the scaler allows it. It refuses jobs that come from fork pull requests (see [Job gate](#job-gate)).
- The scaler passes no AWS, cluster, or GitHub credentials into containers. Jobs get AWS credentials themselves through GitHub OIDC.
- A launchd LaunchAgent starts the scaler at login and restarts it after a crash.
- The setup commands `app-link`, `init`, `add-repo`, `remove-repo` and `doctor` create the App registration link, write the config, add and remove repositories in it, and check it (see [Commands](#commands)).

## Requirements

- macOS on Apple Silicon. Images are `linux/arm64` only.
- Docker Desktop. The scaler connects to `/var/run/docker.sock` and ignores `DOCKER_HOST`. Docker Desktop creates that socket when **Settings > Advanced > Allow the default Docker socket to be used** is on (changing it requires your password).
- Docker must not answer on `127.0.0.1:2375` (see [Docker TCP 2375](#docker-tcp-2375)).
- Go 1.27.3 or later, to build.
- A credential for each target repository: a GitHub App with the Repository permission **Administration: Read and write**, or a personal access token with the matching scope or permission (see [GitHub setup](#github-setup)).

## Quick start

The first setup goes in this order: `app-link` → install the App → `image/build.sh` → `init` → `doctor` → `run` or `service install`.

```sh
# Build the binary.
go build -o bin/actions-runner-scaler ./cmd/actions-runner-scaler

# 1. Print the App registration link (add -org <organization> for an organization's App).
bin/actions-runner-scaler app-link

# 2. Open the link, enter a name, create the App, generate a private key,
#    and install the App on the repositories the scaler serves.
#    Keep the key outside this repository, with mode 0600.
mkdir -p -m 700 ~/.config/actions-runner-scaler
mv ~/Downloads/<app-key>.pem ~/.config/actions-runner-scaler/
chmod 600 ~/.config/actions-runner-scaler/<app-key>.pem

# 3. Build the runner image and pull the BuildKit image.
image/build.sh

# 4. Write the config. init fills runner.image with the ID of the labeled image
#    of step 3 and buildkit.image with the pinned value.
bin/actions-runner-scaler init \
  -app-client-id <client-id> \
  -app-private-key-file "$HOME/.config/actions-runner-scaler/<app-key>.pem" \
  -repo https://github.com/<owner>/<repo>

# 5. Check the config, the credential, GitHub and Docker. Exit 0 means no check failed.
bin/actions-runner-scaler doctor

# 6. Run in the foreground ...
bin/actions-runner-scaler run
# ... or, instead, install the LaunchAgent.
# bin/actions-runner-scaler service install
```

With a personal access token instead of an App, skip steps 1 and 2, store the token in a file (see [Personal access tokens](#personal-access-tokens)), and run `init -token-file <absolute path> -repo <URL>`. Without flags, `init` asks for each missing value on the terminal.

Then point a workflow of that repository at the runners with `runs-on: linux-arm64-deploy` (see [Workflow requirements](#workflow-requirements)).

## GitHub setup

The owner of each target repository (an organization owner, or the personal account) provides the credential and, for the App, installs it. The scaler cannot do it, and a target that is not set up stops with an error in `status` (see [Troubleshooting](#troubleshooting)).

### Choosing a credential

Use them in this order:

1. **A GitHub App.** The `actions/scaleset` library documents App credentials as the preferred choice and personal access tokens as "Simpler but broader scoped", and says to always prefer App credentials. An App is not tied to a person's account, and its REST rate limit is counted per installation.
2. **A fine-grained personal access token for one repository.** It reaches the resources of one owner only, and you choose the one repository and the one permission it needs. It is tied to the person who made it, and its REST rate limit is that person's limit (5,000 requests per hour), shared with all of that person's tokens and apps.
3. **A classic personal access token.** Its `repo` scope grants full access to every public and private repository the person can reach, including write access to their code. Use it only when neither of the above is possible.

What each credential needs:

| Credential | Needs | Reaches |
|---|---|---|
| GitHub App | the Repository permission **Administration: Read and write**; installed on the owner, with the repository in the installation's selection | the repositories of the installations |
| Fine-grained token | Resource owner = the repository's owner; repository access that includes the repository (choose **Only select repositories** and the one repository); the Repository permission **Administration: Read and write** | one owner only |
| Classic token | the `repo` scope, and admin access of the token's person to the repository | every repository the person can reach |

GitHub's token formats show the kind by prefix: `github_pat_` is a fine-grained token, `ghp_` a classic one. `doctor` treats every token that does not start with `github_pat_` as classic.

A fine-grained token with only Administration: Read-only passes the scaler's start-up check (it can list runners) and then fails when the scale set is created, with the library's error in the target's last error.

### Repositories of several owners and personal accounts

- The config holds one default credential, so one App at most. The recommended default is one private App per owner, owned by that owner: `app-link` makes the App private (`public=false`), and only its owner can install it. To serve repositories of another owner from the same scaler, give those repositories a `targets.token-file`, or create one App with `app-link -public`, which lets any account install it on its own repositories. Whether to make the App public is your decision.
- A fine-grained token reaches one owner only, so repositories of two owners need two fine-grained tokens, each in its own `targets.token-file`.
- A repository of a personal account has one owner with full control; collaborators have no admin access to it, and fine-grained tokens cannot be used for repositories where their person is a collaborator. A token for such a repository must be made by the account that owns it. The App can also be installed on a personal account, if it is that account's own App or a public one.

### Creating and installing the App

1. **Create the App** with the link [`app-link`](#app-link) prints. The link selects the Repository permission Administration: Read and write, turns webhooks off, makes the App private unless you pass `-public`, and sets this repository as the Homepage URL. You enter the App's name, which must be unique on GitHub; you can still change the selected values on the page. Whether the page shows those values selected has not been checked in a browser yet.
2. **Generate a private key** on the App's settings page and store it outside the repository, for example `~/.config/actions-runner-scaler/<app-key>.pem`, with mode `0600` or `0400`. You need the App's client ID and this key file.
3. **Install the App on the owner of each repository.** If the installation is limited to selected repositories, the repository must be in the selection. If you add the Administration permission to an App that is already installed, the owner must approve the changed permissions. An installation lookup for a repository that is not covered answered 404 in the real check of 2026-11-26. A missing installation shows as `GitHub App is not installed on repository <owner>/<repo>`, a missing permission as `GitHub App installation on repository <owner>/<repo> lacks administration: write`.

There is no installation ID to configure: the scaler looks up the App's installation on each App target at start. The Organization permission "Self-hosted runners" and the Repository permission "Actions: Read" that earlier versions used are no longer used. They may stay on the App. Removing them also changes every other installation of that App, so that is your decision.

### Personal access tokens

- Create the token on GitHub with the scope or permissions in the table above.
- Store it alone in a file: one line, at most 4 KiB, mode `0600` or `0400`, at an absolute path outside the repository. To keep the token out of your shell history, create the file first with `install -m 600 /dev/null <path>` and paste the token into it with an editor.
- Put the file's path in the config, never the token: `github.token-file` for the default credential, or `token-file` in a `[[targets]]` entry for that repository only. A value that starts with a token prefix is refused as a pasted token (see [Credential and token file rules](#credential-and-token-file-rules)).
- **Expiry.** Tokens can expire. The scaler reads each token file once, at start, and does not warn before a token expires. `doctor` shows the expiry date GitHub reports for the token and warns when it is 7 days away or past. When a token has expired or been revoked, its targets get HTTP 401 at their next GitHub call and go to `오류 대기` (see [Target errors](#target-errors)).
- **Changing a token.** After you replace the content of a token file, or point the config at another file, restart the scaler (see [Restart](#restart)).
- **Organization policies.** An organization can block classic tokens, or require its approval for fine-grained tokens. Both show as a refused check in `doctor` and as the target's error. When GitHub's answer names the permissions it accepts, or says that the organization requires SAML SSO authorization of the token, the error says so.

### Runner group

There is no runner group setting. The scale set of every repository goes into runner group ID 1, the default group, and there is no config key for the group. Creating a scale set in group 1 worked for repositories of an organization and of a personal account in the real check of 2026-11-26. Whether group 1 serves public repositories is not checked yet.

### Public repositories

GitHub advises against self-hosted runners for public repositories. If you list a public repository in `[[targets]]` anyway, the scaler refuses only jobs from fork pull requests (see [Job gate](#job-gate) for what it does not stop). Use GitHub's own settings for the rest: require approval for workflow runs from fork pull requests, and, where your plan offers workflow execution protections, limit the events a repository may run, for example `pull_request_target` and `workflow_run`.

### Adding, removing, moving or renaming a repository

**Adding.** Give the repository a credential (install the App on it, or make a token), run [`add-repo`](#add-repo-and-remove-repo), check with `doctor`, and restart the scaler.

**Removing.** Scale sets are not deleted when the scaler stops or when you remove a target from the config. Remove a repository in this order:

1. Stop the scaler (`service stop`).
2. Run [`delete-scaleset`](#delete-scaleset) for the repository, while it is still in `[[targets]]`.
3. Run [`remove-repo`](#add-repo-and-remove-repo) for it.
4. Start the scaler again (see [Restart](#restart)).

If you already removed the repository, put it back first (`add-repo`) and run `delete-scaleset`. Otherwise the old scale set stays on GitHub with no scaler listening to it, and jobs for the label in that repository could be assigned to it and wait.

**Renaming or moving.** Delete the scale set with the old URL while the old URL is still in the config, then change the URL to the new name. What GitHub answers for the old name of a renamed repository (a 301 redirect is documented) and whether registration through the old URL works is unverified.

## Building

### Binary

```sh
go build -o bin/actions-runner-scaler ./cmd/actions-runner-scaler
# optional version string
go build -ldflags "-X main.version=v0.1.0" -o bin/actions-runner-scaler ./cmd/actions-runner-scaler
bin/actions-runner-scaler version
```

### Images

```sh
image/build.sh
```

The script does two things:

- It pulls the BuildKit image pinned by index digest at the top of `image/build.sh` (`moby/buildkit:<version>-rootless@sha256:…`).
- It builds the runner image from `image/Dockerfile`: the official `actions-runner` base pinned by digest, plus the aws CLI v2, kubectl, the docker CLI, and buildx, with no Docker daemon. It tags the image `actions-runner-scaler-runner:local` and labels it `org.opencontainers.image.source=https://github.com/dewevrei/actions-runner-scaler` and `org.opencontainers.image.title=actions-runner-scaler-runner`. These labels replace the `source` label the base image passes down.

It checks that both images are arm64 and prints the two config values:

```
# scaler config values
runner.image = "sha256:<runner-image-id>"
buildkit.image = "moby/buildkit:<version>-rootless@sha256:<index-digest>"
```

The scaler never pulls. Both images must stay in the local image store, so do not prune them.

To change the BuildKit version, set `BUILDKIT_IMAGE` in `image/build.sh` and the constant `BuildKitImage` in `internal/docker/image.go` to the same value (a test fails when they differ), rebuild the binary, run `image/build.sh` again, and put the new `buildkit.image` in the config. `init` uses the value built into the binary.

`init` fills `runner.image` with the ID of the `actions-runner-scaler-runner:local` image only when it has both labels with exactly these values, and `doctor` reports a `runner.image` without them as `확인 못 함`. A runner image built before the labels were added has none, so `init` does not fill it and `doctor` shows `확인 못 함` until you run `image/build.sh` again. The scaler itself runs with an unlabeled image. The labels are a check against a tag moved onto another image by mistake, not proof of where an image came from: anyone can set them.

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

- The repository must be listed in `[[targets]]` and have a working credential (see [GitHub setup](#github-setup)). A job reaches only the scale set of its own repository.
- `runs-on` is the `scaling.label` value. It is the same in every repository.
- Jobs started by a fork pull request are refused (see [Job gate](#job-gate)).
- Jobs that use `environment:` are not supported, because the OIDC subject changes.
- Build and push with `docker buildx build --push`. The runner has no Docker daemon, and its default builder already points at the runner's BuildKit.
- The runner has the aws CLI v2, kubectl, git, the docker CLI, and buildx (arm64).

## Configuration

The default path is `~/.config/actions-runner-scaler/config.toml`. Every command that reads it also takes `-config <path>`. Write it with [`init`](#init), or start from `config.example.toml`.

The file is TOML:

- Unknown keys, malformed values, and out-of-range values are all reported together. A TOML syntax error is reported alone, with its line and column.
- On any of these errors the scaler enters error-wait.
- Secrets are referenced by path only.

| Key | Default | Allowed values |
|---|---|---|
| `github.app-client-id` | none | non-empty. Set together with `github.app-private-key-file`, or leave both out. Cannot be combined with `github.token-file` |
| `github.app-private-key-file` | none | absolute path to an unencrypted RSA PEM, at most 64 KiB, mode `0600` or `0400` (no group/other bits, no setuid/setgid/sticky). Set together with `github.app-client-id` |
| `github.token-file` | none | absolute path to a personal access token file (see [Credential and token file rules](#credential-and-token-file-rules)). The default credential of every target without its own `token-file`. Cannot be combined with the App keys |
| `runner.image` | required | local image ID `sha256:<64 hex>` |
| `runner.cpus` | `1` | at least 0.01 |
| `runner.memory` | `1g` | at least `256m`. Digits with an optional suffix `b`, `k`, `m`, `g` (binary multiples, either case). No suffix means bytes |
| `runner.pids` | `512` | at least 64 |
| `buildkit.image` | required | `moby/buildkit:<version>-rootless@sha256:<64 hex>` |
| `buildkit.cpus` | `2` | at least 0.01 |
| `buildkit.memory` | `2g` | at least `256m` |
| `buildkit.pids` | `1024` | at least 64 |
| `scaling.label` | `linux-arm64-deploy` | 1 to 40 characters of `a-z`, `0-9`, `-`, starting with a letter or digit. Scale set name and `runs-on` label, the same for every target. GitHub's service rejects a scale set name over 64 characters with HTTP 400 (seen in the real check of 2026-11-26); this limit is below that |
| `jobcheck.listen` | `127.0.0.1:8765` | `<loopback IP>:<port>` |
| `shutdown.drain-timeout` | `10m` | Go duration greater than 0 |
| `[[targets]]` | required | one entry per repository, no upper limit |
| `targets.url` | required | `https://github.com/<owner>/<repo>`, no trailing `/` and no `.git`; the owner is an organization or a personal account. Owner and repository names that differ only in case count as the same repository, and listing one twice is an error. `https://github.com/<owner>` (an organization or account URL) is an error. A value with `@` or a token in it is an error (see [Credential and token file rules](#credential-and-token-file-rules)) |
| `targets.max-runners` | `1` | at least 1. The most runners this target has at once. Each target has its own value |
| `targets.runner-group` | removed | the key is an error, even with an empty value: every repository scale set uses runner group ID 1 |
| `targets.idle` | `0` | 0 to this target's `max-runners`. Runners kept waiting for a job |
| `targets.token-file` | none | absolute path to a personal access token file that this target uses in place of the default credential |

The scaler applies no upper limits of its own to `cpus` and `memory`. A CPU value above the Docker VM's CPU count would make Docker reject every container, so the scaler stops it earlier, with `server runner limit is 0` (see [Server limit](#server-limit)).

### Credential and token file rules

- The default credential is either the two App keys or `github.token-file`. A target with its own `token-file` uses that token; every other target uses the default credential.
- The App keys together with `github.token-file` are an error: `github: set either app-client-id and app-private-key-file, or token-file, not both`.
- If there is no default credential, every target needs its own `token-file`. Otherwise the error lists every target without one: `github: no default credential (app-client-id and app-private-key-file, or token-file) for targets without token-file: targets[<i>] (<url>), ...`.
- With no `github.token-file` and only one of the two App keys, the other key gets `is required`.
- `token-file = ""` counts as not set.
- A token file must be a regular file (a symbolic link counts as its target) at an absolute path, at most 4 KiB, with the App key's mode rule (`0600` or `0400`, no special bits). Its content, less one trailing `\n` or `\r\n`, must be one non-empty line without spaces or control characters. An error names the key and the broken rule, and shows neither the path nor the content.
- A token file value that starts with `ghp_`, `github_pat_`, `gho_`, `ghu_`, `ghs_` or `ghr_` is refused before the file is opened, with `value looks like a token, not a file path; set the path of the token file`. The value is not shown anywhere.
- A repository URL value (`targets.url`, `init -repo`, and the URL argument of `add-repo`, `remove-repo` and `delete-scaleset`) that contains `@`, or one of those prefixes followed by 20 or more of `A-Z`, `a-z`, `0-9`, `_`, is refused before any other check with `value contains a credential; set the repository URL only`, and the value is not shown anywhere.
- The scaler reads token files once, at start. Restart it after a token changes.

`config.example.toml` uses the App as the default credential and shows the two token keys as comments.

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

### Rate limits

After GitHub answers a call with a rate limit, the scaler waits before it repeats calls of the same kind. The waits are separate by who is limited:

- Calls to GitHub's Actions service through a target's scale set client (scale set calls, message sessions, acquiring jobs, issuing a runner configuration, looking up and removing a runner) wait per target. A limit one target receives does not delay the same calls of another target, even when both repositories share one installation or one token.
- Calls to GitHub's REST API (the repository's runner list at start and the lookup of a runner by name) wait per installation for App targets and per token for token targets. Targets of the same installation or the same token share that wait, and targets of another installation or token are not delayed. A token target's start-up check is one of these calls.
- The App's installation lookup at start has one wait for the whole scaler. Token targets do not use it.

GitHub counts a token's REST limit per person, but the scaler does not know whose token it is. Two tokens of the same person therefore wait separately, and each may receive one more rate-limit answer.

### Migrating from earlier versions

**From the App-only configuration.** A config with `app-client-id` and `app-private-key-file` under `[github]` and no token keys is read and checked as before, and every target uses the App. Nothing needs to change.

**From the organization-level version.** Each target must now be one repository. The scaler enters error-wait, and `status` shows the error, in these cases. Nothing is converted for you.

| Organization-level version | Now |
|---|---|
| `targets.url = "https://github.com/<org>"` | error `targets[<i>].url: "<value>" names an organization or account; list each repository as its own [[targets]] url = "https://github.com/<owner>/<repo>" and set its max-runners (default 1)`. Write one `[[targets]]` entry with the repository URL for each repository |
| `targets.runner-group` (any value, empty included) | error `targets[<i>].runner-group: no longer used; repository scale sets always use the default runner group, remove the key`. Delete the key |
| `targets.max-runners` not set (was 2) | now 1, with no error. A target that needs two runners at once must set `max-runners = 2` |
| the same repository twice, in any case | error `targets: repository listed more than once (owner and repository names compare without case): <url>, <url>` |

The organization-level scale set of that version was deleted before the repository-level version (`delete-scaleset` on 2026-11-26), and the current version has no command that deletes an organization-level scale set. If one still exists on GitHub, the scaler does not touch it.

**From 0001.** The 0001 format (one `[[pools]]` entry with a repository URL and a workflow allow-list) is rejected: the scaler enters error-wait, and the error says what to move. Nothing is converted silently, because `allowed-workflows` and `allowed-events` would otherwise be ignored without your knowing.

| 0001 | Now |
|---|---|
| `[[pools]]` | removed. The error reads `pools: [[pools]] is the 0001 format; move max to [[targets]] max-runners, idle to [[targets]] idle, runner-*/build-* to [runner]/[buildkit] cpus, memory, pids, name to [scaling] label, and set [[targets]] url = "https://github.com/<owner>/<repo>" for each repository; allowed-workflows and allowed-events no longer exist` |
| `pools.name` | `scaling.label` |
| `pools.repo-url` | `targets.url`, the same repository URL |
| `pools.max` | `targets.max-runners` (default 1; the 0001 default was 2) |
| `pools.idle` | `targets.idle` |
| `pools.runner-cpus`, `runner-memory`, `runner-pids` | `runner.cpus`, `runner.memory`, `runner.pids` |
| `pools.build-cpus`, `build-memory`, `build-pids` | `buildkit.cpus`, `buildkit.memory`, `buildkit.pids` |
| `pools.allowed-workflows`, `pools.allowed-events` | removed, with no replacement (see [Job gate](#job-gate)) |
| `github.app-installation-id` | removed. The error reads `github.app-installation-id: no longer used; the installation is looked up for each target` |

## Commands

```
bin/actions-runner-scaler <command> [flags]
```

| Command | What it does | Changes |
|---|---|---|
| `run [-config <path>]` | runs the scaler in the foreground | GitHub registrations, Docker containers |
| `status` | prints the running scaler's state | nothing |
| `init [flags]` | writes a new config file | the new config file (and its folder) |
| `add-repo [flags] <URL>` | adds a repository to the config file | the config file, its `.lock` file |
| `remove-repo [-config <path>] <URL>` | removes a repository from the config file | the config file, its `.lock` file |
| `doctor [-config <path>]` | checks the config file and what it depends on | nothing |
| `app-link [-org <organization>] [-public]` | prints the link that creates the GitHub App | nothing |
| `delete-scaleset [-config <path>] <URL>` | deletes a configured repository's scale set and its runners | GitHub |
| `service install\|stop\|uninstall [-config <path>]` | manages the LaunchAgent (see [Running as a service](#running-as-a-service)) | the plist, the running scaler |
| `version` | prints the version | nothing |

A usage mistake exits 1: a flag error prints the error and the command's flags, a missing or extra argument prints the command's usage line. `help`, `-h`, `-help` and `--help` print the command list on stdout and exit 0. No command, or an unknown one, prints the list on stderr and exits 1. Commands that talk to Docker use the Docker Engine client, which negotiates the API version with a ping (`HEAD /_ping`, then `GET /_ping` when the `HEAD` fails or does not answer 200) before its first request.

### run

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

At start, an App target looks up the App's installation on its repository and checks its Administration permission. A token target instead calls `GET /repos/<owner>/<repo>/actions/runners?per_page=1` with its token; transient errors are retried.

### Target errors

A permanent error of one target's GitHub calls stops only that target, so one repository's setup problem does not stop the others. Examples are a repository without the App installed (or outside the installation's selected repositories), a missing Administration permission, and a token that GitHub refuses:

- `personal access token for repository <owner>/<repo> was refused (401): it is invalid, expired or revoked`
- `personal access token for repository <owner>/<repo> cannot manage its runners (<code>): a classic token needs the repo scope and admin access, a fine-grained token needs Administration: Read and write on this repository` (for 403 that is not a rate limit, and 404). When GitHub's answer names the permissions it accepts, `; GitHub says the endpoint accepts <value>` follows; when an organization requires SAML SSO authorization of the token, `; an organization requires SAML SSO authorization of this token` follows.

What happens:

- At start, the target goes straight to `오류 대기`.
- While running, the target goes to `오류 drain 중`: it starts no runners, its idle runners are removed, and its busy runners finish (up to `shutdown.drain-timeout`). Then it goes to `오류 대기`.
- In `오류 대기` the scaler retries the target's start-up calls, first after 30 s, then at doubling intervals up to every 30 minutes, with no limit on the number of tries. When the cause is fixed on GitHub, the target returns to `정상` without a restart. `status` shows the time of the next try. A new token in the token file is not read by these retries; restart the scaler after a token change.
- A signal drain stops the retries.

Config errors, Docker problems, the lock and the job gate's listen address stop the whole scaler instead (`상태: 오류 대기`), and need a restart after you fix them.

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
https://github.com/<owner>/<repo>: 정상, 실행 중 1, 대기 0, 대기 job 0, 최대 1, 연속 시작 실패 0
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
https://github.com/<owner>/<repo>: 오류 대기, 실행 중 0, 대기 0, 대기 job 0, 최대 1, 연속 시작 실패 0, 오류: GitHub App is not installed on repository <owner>/<repo> (<time>), 다음 시도 <time>
```

### init

```sh
bin/actions-runner-scaler init [-config <path>] \
  [-app-client-id <id> -app-private-key-file <path> | -token-file <path>] \
  [-repo <URL>[=<max-runners>]]... [-runner-image <id>] [-buildkit-image <ref>]
```

`init` writes a new config file at `-config` (default `~/.config/actions-runner-scaler/config.toml`).

| Flag | Value |
|---|---|
| `-app-client-id`, `-app-private-key-file` | the App's client ID and the absolute path of its private key file |
| `-token-file` | the absolute path of a token file, as the default credential instead of an App. Cannot be combined with the App flags |
| `-repo` | a repository URL, optionally `=<max-runners>` (default 1). Repeat it for more repositories |
| `-runner-image` | `runner.image`. Default: filled from the local `actions-runner-scaler-runner:local` image, see below |
| `-buildkit-image` | `buildkit.image`. Default: the image built into the binary, which is the one pinned in `image/build.sh` |

What it does:

1. If a file (or a symbolic link) is already at the path, it writes nothing, prints `config file <path> already exists; edit it, or use add-repo and remove-repo` on stderr and exits 1. There is no overwrite flag; edit the file, or use `add-repo` and `remove-repo`.
2. It checks every flag: the repository URLs with the rules of `targets.url`, `max-runners` (an integer of 1 or more), the key file and the token file with the rules of the config. Any problem: nothing is written, one line per problem goes to stderr (a `-repo` value is named `-repo #<n>` and not quoted), exit 1.
3. When an image flag is missing, it looks the images up in the local Docker. If `actions-runner-scaler-runner:local` exists and has both labels of `image/build.sh`, its ID becomes `runner.image`. Without `-buildkit-image` it uses the image built into the binary, the one pinned in `image/build.sh`. It says on stdout what it filled and why it did not:

   ```
   runner.image: <id> (actions-runner-scaler-runner:local의 ID)
   actions-runner-scaler-runner:local 이미지가 로컬에 없어 runner.image를 채우지 못함: image/build.sh를 먼저 실행
   actions-runner-scaler-runner:local 이미지에 이 프로젝트의 라벨이 없어 runner.image를 채우지 못함: 라벨은 image/build.sh가 붙이므로 다시 실행
   buildkit.image: <ref>
   buildkit.image가 로컬에 없음: image/build.sh를 실행하면 받아 옴
   Docker 이미지 확인 실패: <error>
   ```

   If Docker does not answer, `runner.image` counts as missing.
4. When a required value is still missing (a credential, at least one repository, `runner.image`):
   - On a terminal it asks, in this order: `자격 [app/token] (기본 app):`; for an App the note `GitHub App이 없으면 먼저 actions-runner-scaler app-link로 만든다.`, then `GitHub App client ID:` and `App 개인키 파일 경로:`; for a token `토큰 파일 경로:`; `저장소 URL (빈 줄이면 끝):` and `max-runners (기본 1):` until an empty line; `runner.image (image/build.sh가 출력한 값):`. An answer that breaks a rule gets its error and the same question again. It asks for secrets only as file paths. If input ends before every answer, it prints `input ended before every value was given; nothing written` on stderr and exits 1.
   - When stdin is not a terminal, it writes nothing, prints one line per missing value naming its flag (for example `missing runner.image (run image/build.sh first): set -runner-image`) on stderr and exits 1.
5. It writes the file with mode `0600`, in the order and with the comments of `config.example.toml`, with your values and every defaulted key marked `# default`. The content is written to a temporary file in the same folder and linked into place, so the path never holds a partial file, a file created there meanwhile is not overwritten, and no temporary file is left. If the folder is missing and its parent exists, `init` creates that one folder with mode `0700`; if the parent is missing too, it creates nothing and exits 1.
6. On success it prints `설정 파일을 만듦: <path>` and `다음: actions-runner-scaler doctor -config <path>`, and exits 0.

`init` makes no GitHub call and changes nothing outside the config file and its folder. Its only Docker requests are the image lookups (`GET /images/{name}/json`) and the client's version ping, and only when an image flag is missing.

### add-repo and remove-repo

```sh
bin/actions-runner-scaler add-repo [-config <path>] [-max-runners <n>] [-idle <n>] [-token-file <path>] <repository URL>
bin/actions-runner-scaler remove-repo [-config <path>] <repository URL>
```

`add-repo` appends one blank line and one `[[targets]]` block to the end of the file: `url`, `max-runners` (1 when not given), and `idle` and `token-file` only when given. Every earlier byte (comments, key order, blank lines) stays as it was. `-max-runners` must be at least 1, `-idle` from 0 to `max-runners`, and `-token-file` must pass the token file rules.

`remove-repo` deletes the block of the target whose owner and repository match without case: from its `[[targets]]` header line through the last key line before the next table header. Comment lines above the header and blank or comment lines after the last key stay, since they may belong to the next block. All other bytes stay as they were.

On success both print the repository URL and `스케일러를 다시 시작해야 반영됨` on stdout and exit 0. `remove-repo` adds `그 저장소의 scale set은 GitHub에 남음. 지우려면 그 저장소를 다시 넣고, 스케일러를 멈춘 뒤 delete-scaleset을 실행`, because it changes only the file: see [Adding, removing, moving or renaming a repository](#adding-removing-moving-or-renaming-a-repository) for the order that deletes the scale set first. If the edited file still has problems unrelated to that repository (for example an image value), the change is still made and the problems are listed on stdout under `config still has problems:`.

They change nothing, print `add-repo: <error>` or `remove-repo: <error>` on stderr, and exit 1 when:

- the file is not TOML
- the URL is not a repository URL of the `targets.url` form, or is an organization URL
- `add-repo`: the repository is already in the file, or a flag breaks its range or the token file rules
- `remove-repo`: the repository is not in the file, or it is the last target
- the file already lists any repository more than once
- the targets are not written as `[[targets]]` header blocks (an inline array or dotted keys, for example), or reading the edited file back does not give the original list plus or minus that one target. In this case `add-repo` also prints `add this block to the config file by hand:` and the block to paste, made only from its arguments (a `token-file` holds the path, never the token).

The edited content goes to a temporary file in the same folder, with the original file's mode, and is renamed over the original. If the config path is a symbolic link, the real file is replaced and the link stays. If writing or renaming fails, the original stays and no temporary file is left.

**Lock file.** Before reading, both commands take an exclusive lock on `<config>.lock`, a file next to the real config file (for `config.toml`, `config.toml.lock`), created with mode `0600` if missing and never removed. If another `add-repo` or `remove-repo` holds it, the command prints `Blocking: waiting for file lock on <lock path>` on stderr and waits, so two commands run one after the other and neither change is lost. A lock path that is a symbolic link or not a regular file is an error. An editor does not take this lock: if you run these commands while the file is open in an editor, whichever saves last wins.

Neither command takes the scaler's lock, calls GitHub or Docker, or changes anything besides the config file and its lock file, so you can run them while the scaler runs. The scaler reads its config once, at start, so restart it to apply the change.

### doctor

```sh
bin/actions-runner-scaler doctor [-config <path>]
```

`doctor` runs every check below, in this order, and prints one line per check: `<result> <check>: <summary>`. The result is `통과` (passed), `실패` (failed) or `확인 못 함` (could not check). A line that is not `통과` is followed by `  원인: <cause>` lines and a `  고치는 법: <fix>` line, and any line can be followed by `  경고: <warning>`. The last line counts the results: `통과 <n>, 실패 <n>, 확인 못 함 <n>`. It exits 1 when at least one check is `실패`, and 0 otherwise; `확인 못 함` does not change the exit code, because it can come from a GitHub outage or rate limit as well as from the config.

| Check | What it checks |
|---|---|
| `설정` | every error of the config file except the key and token file errors. `실패` when the file cannot be read |
| `App 키 파일` | the App private key file (only when the App key is set) |
| `토큰 파일 <key>` | each token file, named by its key (`github.token-file`, `targets[<i>].token-file`), never by path |
| `GitHub <owner>/<repo>` | per target, with its credential. App: the installation lookup and the Administration permission. Classic token: the runner list call and the `repo` item of the `X-OAuth-Scopes` header. Fine-grained token: the runner list call, then `GET /repos/<owner>/<repo>/actions/policies?per_page=1`, which needs Administration write |
| `Docker` | the Docker ping on `/var/run/docker.sock` |
| `Docker TCP 2375` | the check of [Docker TCP 2375](#docker-tcp-2375) |
| `이미지 runner.image`, `이미지 buildkit.image` | the image is local and `arm64`; `runner.image` also has the labels of `image/build.sh` |
| `서버 한도` | the server limit is above 0 and not below the sum of `idle`. The summary shows the limit |

- A check whose input failed an earlier check is `확인 못 함`, and its cause names that check. All other checks still run, so one run shows every problem.
- A runner image without the labels of `image/build.sh` is `확인 못 함`, not `실패`: the scaler runs with it. Run `image/build.sh` again and put the new ID in the config to make it `통과`.
- Each GitHub and Docker call is made once with a 10 s limit and no retry. A timeout, transport error, 5xx or rate limit makes the check `확인 못 함`; for a rate limit, the cause gives the time after which it can be run again.
- For a token target, the summary ends with `, 토큰 만료 <value>` when GitHub reports the token's expiry. When the expiry is 7 days away or past, `  경고: 토큰이 <value>에 만료됨. 새 토큰을 만들어 파일을 바꾸고 스케일러를 다시 시작한다` follows. The warning changes neither the result nor the exit code.
- Paths of key and token files and token values never appear in the output. GitHub and config errors in the causes are in English, as in the log.

Example (placeholders):

```
통과 설정: 문제 없음
통과 App 키 파일: github.app-private-key-file 읽음
통과 토큰 파일 targets[1].token-file: 읽음
통과 GitHub <owner>/<repo>: App 설치 <id>, administration: write
통과 GitHub <owner>/<other-repo>: classic 토큰, repo scope, 토큰 만료 <value>
통과 Docker: /var/run/docker.sock 응답함
통과 Docker TCP 2375: 127.0.0.1:2375에 Docker API가 없음
확인 못 함 이미지 runner.image: 확인하지 못함
  원인: 이 프로젝트의 image/build.sh로 만든 이미지인지 알 수 없음(라벨 없음)
  고치는 법: image/build.sh를 실행하고 그 출력의 값을 설정에 적는다
통과 이미지 buildkit.image: 로컬에 있음, arm64
통과 서버 한도: runner 2개
통과 9, 실패 0, 확인 못 함 1
```

**Safe while the scaler runs.** `doctor` sends only GET requests to GitHub: the App's installation lookup with the App JWT, and the runner list and policies calls with a token. It creates no installation token, registration token, runner configuration, scale set or message session. Its Docker requests are the ping (`HEAD /_ping`, then `GET /_ping` when that fails or does not answer 200; counted as one call), `GET /info`, `GET /images/{name}/json`, and the TCP 2375 check. It lists and removes no containers, takes neither the scaler's lock nor the status socket, and only reads files. Its GitHub calls count against the same rate limits as the scaler, once per target.

### app-link

```sh
bin/actions-runner-scaler app-link [-org <organization>] [-public]
```

`app-link` prints the GitHub App registration URL on its first line:

```
https://github.com/settings/apps/new?url=https://github.com/dewevrei/actions-runner-scaler&public=false&webhook_active=false&administration=write
```

With `-org <organization>` the URL starts with `https://github.com/organizations/<organization>/settings/apps/new`; with `-public`, `public=true`. After a blank line it prints numbered next steps in Korean: enter a name, create the App, generate a private key and save it with mode 0600, install the App on the repositories, and run `init`. The steps also say that another owner's repositories need `-public` or a `token-file`, and recommend one private App per owner.

`-org` must match `[A-Za-z0-9-]+`; otherwise nothing is printed on stdout, the error goes to stderr, and it exits 1. The command reads no config file, makes no network call and writes no file.

### delete-scaleset

```sh
bin/actions-runner-scaler delete-scaleset [-config <path>] <repository URL>
```

This deletes one target repository's scale set and its runner registrations. `<repository URL>` is the `targets.url` value of a repository that is listed in `[[targets]]`, for example `https://github.com/<owner>/<repo>`; owner and repository names are compared without case. The command reads and validates the config like `run`. For an App target it looks up the App's installation on that repository and checks its permission; for a token target it makes the token's runner list call instead (retrying transient errors) and deletes with that token, so a config with tokens only needs no App key. Then it deletes the scale set named `scaling.label` in runner group ID 1 of that repository, together with the registrations of its runners. Other runners of the repository and other targets are not touched. On success it prints `scale set 삭제됨: <label> (ID <id>), runner 등록 <n>개 제거`. If no scale set has that name, it prints `scale set 없음: <label>. 지울 것이 없음` and exits 0.

**It deletes only a repository that is in `[[targets]]`.** A scale set that someone else or another scaler created with the same credential is never deleted by this command. So run it before you remove a repository from the config (see [Adding, removing, moving or renaming a repository](#adding-removing-moving-or-renaming-a-repository)). If you already removed the repository, put its `[[targets]]` entry back and run the command. For a renamed or moved repository, run it with the old URL while the old URL is still in the config, then change the config to the new URL.

**It needs the whole scaler stopped.** The command takes the same lock as `run`, so one target's scale set cannot be deleted while the scaler runs. Stop it with `service stop`, which drains every target: no new jobs are taken, and the busy jobs of the other repositories are waited for too, for up to `shutdown.drain-timeout`. Start the scaler again afterwards (see [Restart](#restart)).

It deletes nothing and exits 1 in these cases:

- A scaler holds the lock, including one in error-wait. Stop it first with `service stop`.
- The config does not load or fails validation. The error goes to stderr.
- The URL contains a credential. The error `delete-scaleset: value contains a credential; set the repository URL only` does not quote it.
- The URL is not in the config, an organization URL or a repository that you removed included. The error names the config path and the configured URLs, and nothing is looked up on GitHub:

  ```
  target "<URL>" is not in the config <config path> (targets: <url>, <url>, ...); delete-scaleset deletes only a repository listed in [[targets]]
  ```

- The installation lookup or the permission check fails (for example `GitHub App is not installed on repository <owner>/<repo>`), or GitHub refuses the token (the texts of [Target errors](#target-errors)). The error goes to stderr.
- More than one scale set has the label's name. Delete the extras so only one is left, then run again.
- A runner container of that scale set is still alive. The command lists those runners. Remove the containers, then run again:

  ```sh
  docker ps -aq --filter label=managed-by=actions-runner-scaler --filter label=kind=runner | xargs docker rm -f
  ```

  The BuildKit containers and networks left behind are removed at the next scaler start.

A normal stop never deletes a scale set, and neither does removing a target from the config.

### version

```sh
bin/actions-runner-scaler version
```

Prints the version string set at build time with `-ldflags "-X main.version=<version>"`.

## Running as a service

```sh
bin/actions-runner-scaler service install [-config <path>]
bin/actions-runner-scaler service stop [-config <path>]
bin/actions-runner-scaler service uninstall [-config <path>]
```

`stop` and `uninstall` read only `shutdown.drain-timeout` from the config, to know how long to wait. Without `-config`, they use the config named in the installed plist, or `~/.config/actions-runner-scaler/config.toml` if there is no plist. If the config does not load, they log a warning and wait for the default 10 minutes. `install` waits for the `drain-timeout` of the config it installs, not the one the running scaler loaded. Before you lower `drain-timeout` and reinstall, run `service stop` first, so the old, longer drain is not cut short by the wait.

Only one of `install`, `stop` and `uninstall` runs at a time. They share a lock file, `~/.local/state/actions-runner-scaler/stop.lock`. A second command prints `another service command is running` and exits 1. Without the lock, two commands could both signal the scaler, and a second signal cuts the drain short.

### Install

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

### Stop

Stop drains first:

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

### Restart

Restart after fixing the cause of error-wait, editing the config (including `add-repo` and `remove-repo`), or changing a token file:

```sh
bin/actions-runner-scaler service stop
launchctl kickstart gui/$(id -u)/io.github.dewevrei.actions-runner-scaler
```

Or run `service install` again. In error-wait, the scaler exits on the first signal, so `stop` returns at once. `kickstart -k` restarts in one step, but it is reported to cut a running drain at 60 s (see above).

## Security model

- **Containers.** Each job runs in its own runner container (non-root, all capabilities dropped, `no-new-privileges`, resource limits, no host mounts, no Docker socket) with its own rootless BuildKit container on a private network, all removed after the job (see [How it works](#how-it-works)).
- **Credentials stay on the host.** The scaler passes no AWS, cluster, or GitHub credentials into containers. Jobs get AWS credentials themselves through GitHub OIDC, and whether a job gets an AWS role is decided by the AWS trust policy.
- **Secrets by path only.** The config holds only the paths of the App key and token files, and no command takes a secret value as a flag or a prompt answer. The scaler refuses a pasted token or key in place of a path, and a repository URL with a credential in it, without printing the value. Key and token files must have mode `0600` or `0400`. Logs redact tokens and keys, and token values never appear in errors, `status`, or the output of any command.
- **Job gate.** The scaler refuses jobs from fork pull requests before any step runs (see [Job gate](#job-gate)).
- **Docker API exposure.** The scaler refuses to start while Docker answers on `127.0.0.1:2375` (see [Docker TCP 2375](#docker-tcp-2375)).

### Job gate

The runner image sets a job-started hook. Before any step runs, the hook sends the job's event name and event file (`GITHUB_EVENT_NAME` and the file `GITHUB_EVENT_PATH` points to) to the scaler at `http://host.docker.internal:<jobcheck port>/v1/event`, using a per-runner token. The scaler decides from that report alone and makes no request to GitHub. The hook lets the job continue only on `allow`. On any other answer it keeps waiting and never lets a step run, including `if: always()` steps.

**What is refused.** The scaler refuses jobs that come from a fork pull request, by comparing repository IDs in the event file:

- the event has a `pull_request` object (whatever the event name) and its head repository, its base repository and the run's repository are not all the same repository
- the event has a `workflow_run` object and the run's head repository and the run's repository are not the same repository. This covers a workflow started by the run of a fork pull request
- the event name is `pull_request`, `pull_request_target`, `pull_request_review` or `pull_request_review_comment` but the event has no `pull_request` object, or it is `workflow_run` without a `workflow_run` object
- a repository ID that is compared is missing, `null`, or not a positive integer
- the report is not exactly one JSON object, its event name is not 1 to 64 lowercase letters and `_`, or its body is larger than 32 MiB

Everything else is allowed: any branch, tag or event, including manual runs and pull requests from branches of the same repository.

On a denial the scaler logs `job denied; waiting for the runner to stop` with `runner`, `event`, `reason` and, when the event names it, `head_repo`. The `reason` is one of `pull-request-from-other-repo`, `workflow-run-from-other-repo`, `pull-request-repo-id-invalid`, `workflow-run-repo-id-invalid`, `pull-request-missing`, `workflow-run-missing`, `payload-not-one-object`, `event-name-invalid` or `payload-too-large`. The job then fails without running any step after "Set up job", and the runner's containers are removed. The same happens when no report arrives within 180 s of the runner turning busy.

**What the gate does not stop.** It looks at neither the person who started a job nor the branch. These are allowed, and are left to GitHub's settings and the repository's access rules (see [Public repositories](#public-repositories)):

- On public repositories: a comment (`issue_comment`, for example a `/deploy` command) on a fork pull request, when the workflow then checks out the pull request; a chain of `workflow_run` workflows that starts from a fork pull request; a review from an outsider on a same-repository pull request; events anyone can cause, such as `watch`, `fork` and `discussion_comment`.
- A re-run of an older run of the default branch by someone with write access. It runs the old commit and, if the AWS trust policy accepts that ref, gets the AWS role.
- In the same repository, `pull_request_target` and `workflow_run` jobs, which run on the default branch.

Whether a job gets the AWS role is decided by the AWS trust policy, not by the scaler. A job on a branch other than `main` runs, but its OIDC step is refused if the policy accepts only `main`.

### Docker TCP 2375

If Docker answers `GET /_ping` on `127.0.0.1:2375`, any container could control the host's Docker through `host.docker.internal`. The scaler checks this at start and enters error-wait with that message. `doctor` makes the same check.

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

## Troubleshooting

- **Start with `doctor` and `status`.** `doctor` checks the config, the credentials, GitHub and Docker in one run without touching the running scaler. In error-wait (`상태: 오류 대기`), the `오류:` line of `status` holds every config error or the permanent error that stopped startup (missing image, Docker TCP 2375, `server runner limit is 0`, authentication). The scaler makes no GitHub or Docker calls in this state. Fix the cause, then restart with `service stop` and `launchctl kickstart` (see [Restart](#restart)). A target in `오류 대기` shows its own error on its line and retries without a restart (see [Target errors](#target-errors)).
- **Read the log:** `~/Library/Logs/actions-runner-scaler/stderr.log`, one JSON object per line, for example `tail -f … | jq .`.
- **`실행 중 아님`:** the agent is not loaded or the process exited. Check the log and `launchctl print gui/$(id -u)/io.github.dewevrei.actions-runner-scaler`.
- **A non-zero `연속 시작 실패`:** runner starts are failing, for example because an image was removed, a CPU or memory value is too large for Docker, or JIT issuance failed. After 3 failures in a row, starts back off from 10 s up to 5 min.
- **A job sits at "Set up job" and then fails:** the gate denied it or never allowed it. Search the log for the runner name to see the reason.
- **A target shows `오류 대기` with `GitHub App is not installed on repository <owner>/<repo>`:** the App is not installed on the owner, or the installation does not include that repository. Fix it on GitHub (see [Creating and installing the App](#creating-and-installing-the-app)). The target retries without a restart.
- **A target shows `GitHub App installation on repository <owner>/<repo> lacks administration: write`:** the App installation on that repository does not have the Repository permission "Administration: Read and write". Either the App does not have it, or the owner has not approved it. After it is granted the target retries without a restart.
- **A target shows `personal access token for repository <owner>/<repo> was refused (401) ...`:** the token is invalid, expired or revoked. Make a new token, replace the file's content, and restart the scaler.
- **A target shows `personal access token for repository <owner>/<repo> cannot manage its runners (<code>) ...`:** the token lacks the `repo` scope (classic), the admin access of its person, or Administration: Read and write on that repository (fine-grained), or an organization policy blocks it. Check the token against [Choosing a credential](#choosing-a-credential); the text after `;`, when present, names what GitHub accepts or that SAML SSO authorization is needed. A fixed permission is picked up by the retries; a new token needs a restart.
- **A fine-grained token target fails when its scale set is created:** the token may have Administration: Read-only. `doctor` shows this as `실패` on the target's `GitHub` line.
- **`doctor` warns `토큰이 <value>에 만료됨`:** make a new token before that date, replace the file's content and restart the scaler.
- **`github: set either app-client-id and app-private-key-file, or token-file, not both`** or **`github: no default credential ...`:** see [Credential and token file rules](#credential-and-token-file-rules).
- **`value looks like a token, not a file path`** or **`value contains a credential`:** a token was pasted in place of a file path or into a URL. Put the token in a file, set its path, and make a new token if the old one may have been seen.
- **`config` errors that name `targets[<i>].url` or `targets[<i>].runner-group`:** the config still has the organization-level format (see [Migrating from earlier versions](#migrating-from-earlier-versions)).
- **`add-repo` or `remove-repo` prints `Blocking: waiting for file lock on <path>`:** another `add-repo` or `remove-repo` is editing the same file; it continues once that one ends.
- **`add-repo` prints `add this block to the config file by hand:`:** the file's targets are not written as `[[targets]]` blocks. Paste the block it prints at the end of the file.
- **A job in a repository that is not in `[[targets]]` gets no runner from this scaler:** the scaler registers nothing there. Add the repository to `[[targets]]` and give it a credential.
- **A job waits for a long time:** `status` shows `대기 job` above 0 when the server limit or the target's `max-runners` leaves no free slot. The job starts when a slot frees up. Raise the limit as described in [Server limit](#server-limit) if this is common.
- **`message poll got no answer` (WARN):** GitHub's service accepted the connection but did not answer a poll for more than 70 s, so the scaler opens a new session. If it repeats, check the network.
- **`message poll rate limited` (WARN):** GitHub limited the polls. The log has `status` and `retryAt`, the time the poll can be repeated.
- **`job no longer available` (INFO):** a job the scaler wanted to take was already taken elsewhere or cancelled. It is dropped from the waiting list. No action.
- **`waiting job dropped` (INFO):** the scaler removed a waiting-list entry that GitHub no longer counts as an assigned job. If GitHub offers or assigns that job again, it is added again.
- **`GitHub counts more available jobs than the scaler knows` (WARN):** GitHub reports more unassigned jobs than the scaler has on its waiting list, so a job message may have been missed. The scaler only logs it and changes nothing.
- **Leftover containers after a crash:** they remain until the next start. At start the scaler removes every container and network it labeled, and the stale registrations of each target repository's scale set.

## Development

```sh
go build ./...
go test -race ./...
```

- Tests with the `integration` build tag call real GitHub and Docker (`go test -tags integration ...`); delete what they create.
- `image/build.sh --record` also builds a diagnostic variant of the runner image (`actions-runner-scaler-runner:record`) whose hook records how the runner started it, without values or repository IDs. See the comment at the top of the script.
- Each change has its design documents in `intent/NNNN-<name>/` (`INTENT.md`, `SPEC.md`, `PLAN.md`). `AGENTS.md` holds the conventions for working in this repository.

## License

MIT License. See [LICENSE](LICENSE).
