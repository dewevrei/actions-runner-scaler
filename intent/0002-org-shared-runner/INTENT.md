# Intent: Shared in-house runner server

Author: dewevrei. Status: accepted.

## Problem

Developers want one in-house workflow server that runs our runner program, so that they can deploy their work through it from many repositories. Change 0001 made deploy-job runners scale automatically (confirmed (evidence: `intent/0001-deploy-runner-autoscale/PLAN.md`, M7 results)), but one scaler serves exactly one repository (confirmed (evidence: 0001 SPEC settings table, pools "exactly one" and one repository URL per pool)), and a second scaler cannot run under the same user (confirmed (evidence: 0001 PLAN M7-8, second instance exits 1 with "cannot take the scaler lock")).

The first version of this change registered runners once per organization, so all repositories of an organization shared one maximum and one repository could take every runner (confirmed (evidence: `internal/github/target.go` accepts only `https://github.com/<org>`; GitHub takes one capacity per scale set, scaleset v0.5.0 `GetMessage` `maxCapacity`)). The user wants each repository, whether an organization's or a personal account's, to have its own maximum number of runners.

## Proposed outcome

- Each repository the server serves has its own scale set and its own maximum number of runners, whether the repository belongs to an organization or to a personal account. Example: on a server with 60 cores and 128 GB, repo1 with a maximum of 5 and repo2 with a maximum of 10 run up to 15 jobs at the same time, 5 from repo1 and 10 from repo2 (that server's limit is at least 15).
- The server never runs more runners at once than its CPU and memory allow, even when the repositories' maximums add up to more than that.
- One server serves repositories of several owners (organizations and personal accounts) at once.
- When 5 deploy jobs from different repositories arrive at the same time, the server creates one single-use runner container per job and runs them, as far as each repository's maximum and the server's limit allow.
- Jobs beyond what a repository's maximum or the server allows wait and start when a runner slot frees up (GitHub cancels a self-hosted job after 24 hours in the queue, confirmed (evidence: https://docs.github.com/en/actions/reference/limits, "Job queue time")).
- Adding a repository is one entry in the scaler settings and a scaler restart; the repository's workflows only need `runs-on`.
- The scaler keeps no workflow allowlist; this replaces 0001's workflow allowlist and its main-branch-only rule.
- Jobs from the listed repositories run on any branch, pull request or manual dispatch, except jobs of pull requests from forks, which fail before their first step. Jobs not on the main branch run but do not get the AWS role, because the trust policy allows only main (confirmed (evidence: 0001 INTENT Problem)).
- The checks carried over from 0001 pass in real runs:
  - after `service stop`, the service stays loaded, and after `launchctl kickstart` the scaler takes jobs again;
  - reinstalling the service while a job runs lets the job finish, and the service comes back loaded;
  - after `service uninstall` during a job, no runner container is left;
  - when the GitHub service stops answering polls, the scaler reports the error within about 6 minutes (5 minutes without an answer plus the uncut poll of about 50 seconds) and takes jobs again after the service recovers;
  - a message that was pending when a poll was cut arrives in a later poll;
  - the number of rate-limit responses to polls during the other real runs is recorded from the scaler logs (no separate long-running test).
- After a host restart, the server takes jobs again within 5 minutes with no human action. This is checked in a later real run, after the team repositories' deploy workflows have moved onto this runner; it is not a completion condition of this change.
- A repository entry that does not state its maximum gets a maximum of 1.
- Out of scope: organization-level registration (one scale set serving many repositories), jobs that use `environment:` (their OIDC subject differs from what the trust policy allows), CI-specific labels, caches and sizing (CI jobs that use this label are not refused), moving the team repositories' existing deploy workflows onto this runner, running and testing on Linux, multiple hosts, VM isolation.

## Affected users and systems

- Developers of the served repositories and their workflows; the owners of those repositories (organization owners or personal-account owners), who install the GitHub App on the repositories.
- The in-house workflow server. Today it is this Mac (macOS on Apple Silicon, Docker Desktop).
- GitHub organizations and personal accounts that own the served repositories: the GitHub App's installation and permissions, and repository-level runner registration.
- The organization-level scale set and runner registrations made by the first version of this change during its real tests, which are no longer used.
- OIDC, the AWS role, the registry and the Kubernetes cluster: this change does not modify them, but jobs reach them. Repositories that the administrator's trust policy does not list run their jobs without the AWS role (confirmed (evidence: 0001 INTENT Problem, the role allows only the registered team repositories)).

## Constraints

- Register runners per repository: each repository gets its own scale set and its own maximum. Reason: the user wants a separate maximum per repository, and organization and personal-account repositories then register the same way; ARC sets a maximum per scale set whose URL may be a repository (confirmed (evidence: ARC `charts/gha-runner-scale-set/values.yaml` lines 2–3 and 60–61 at eca1a62, `githubConfigUrl` example `https://github.com/myorg/myrepo` and `maxRunners`)), runscaler supports repository runners (confirmed (evidence: runscaler `docs/configuration.md` lines 21–27 at 6f33444)), and 0001 already registered at repository level (confirmed (evidence: 0001 SPEC settings table)).
- Each repository's maximum applies within the server's limit, and the server's limit follows its CPU and memory. Reason: the user decided that each repository uses its own maximum while what actually runs follows the server's capacity.
- Adding or removing a repository needs a scaler restart. Reason: the settings are read once at start-up and there is no reload path (confirmed (evidence: `internal/daemon/startup.go`, `start` calls `LoadConfig` once)); the user accepts a restart for this.
- A scale set is deleted only when the user runs the delete command; removing a repository from the settings, or the first version's organization-level scale set, leaves the scale set on GitHub until then. Reason: ARC deletes a scale set only when its resource is deleted and runscaler only through a separate command (confirmed (evidence: ARC `controllers/actions.github.com/autoscalingrunnerset_controller.go` lines 898–903 at eca1a62; runscaler `cmd/runner/scaleset.go` lines 195–212 at 6f33444)).
- An organization URL in the settings is a settings error that says to list repository URLs instead. Reason: only repository-level registration remains.
- No fixed limit on the number of repositories. Reason: ARC and runscaler set none; each served repository's owner installs the GitHub App, and a repository without it is reported as an error.
- Refuse jobs of pull requests from forks. Reason: public repositories accept pull requests from anyone, and code from outside must not reach the internal network or the runner's credentials; runner-up does the same by default (confirmed (evidence: runner-up `internal/config/config.go` at 013cc74, `AllowForkPullRequests` "Off by default")).
- The runner label stays `linux-arm64-deploy` and is meant for deploy jobs; nothing refuses other jobs that use it. Reason: CI goes to its own label later, as decided in 0001 (confirmed (evidence: 0001 INTENT Constraints)).
- The GitHub App needs the repository permission "Administration: Read and write" on every served repository. Reason: repository-level scale sets need it (confirmed (evidence: 0001 SPEC Design, GitHub App permissions, tested 2026-11-20)).
- Linux support must be addable later without redesigning the scaler. Reason: the real server will run Linux.
- Build and test on macOS (launchd, Docker Desktop). Reason: this Mac is the always-on host on the allowed network that runs the scaler today (confirmed (evidence: 0001 INTENT Affected users and systems)).
- AWS access stays with the administrator's OIDC trust policy; the scaler injects no credentials and does not know which repositories may assume the role. Reason: the trust policy lists repositories one by one and adding one is the administrator's process (confirmed (evidence: 0001 INTENT Problem, the role allows only the main branch of the registered team repositories)).
- A job may read its own runner's credentials and JIT configuration; this stays an accepted risk although jobs now run on any branch. Reason: GitHub treats one job as one trust boundary (confirmed (evidence: 0001 SPEC Areas of concern 18)), each runner takes one job and is removed, only people with write access to a listed repository can start its non-fork jobs, and fork pull requests are refused.
- Keep all of 0001's isolation guarantees: non-root runner, no host Docker socket, no privileged containers, per-job CPU and memory limits, no path for job code to control host Docker or other runners, builds in a per-runner rootless build container. Reason: jobs from many people share one server and can reach the internal network.
- Do not bypass the administrator's access restrictions, and do not run runners inside the cluster. Reason: administrator policy, and the team cannot create cluster roles or CRDs (confirmed (evidence: 0001 INTENT Constraints)).
- Keep Go, `github.com/actions/scaleset`, and the GitHub App owned by a personal account. Reason: the library is GitHub's official one and ARC uses it; the App keeps token renewal in the library and separate from personal tokens (confirmed (evidence: 0001 INTENT Constraints)).
- Keep what the first version of this change built where it does not depend on organization-level registration: the server limit from CPU and memory, waiting for a free slot across repositories, the fork refusal, error handling per registration, and the status output. Reason: they worked for one organization in real runs, apart from the fork refusal, whose real runs were deferred (unverified (the real-run results are not recorded in this repository)).
- Run the reboot test after every other outcome works, once the team repositories that will use the server can deploy through our runner. Reason: it verifies the finished behavior on the setup it is meant for and interrupts the host and any running test; moving those workflows is outside this change.
- No internal names and no secret values in the repository. Reason: the repository is public.

## Open questions

- Which per-runner CPU and memory defaults and which test sizes show the per-repository maximums and the server limit on this Mac (10 CPU / 9.70 GiB Docker VM, server limit 3 with the current defaults, confirmed (evidence: scaler start-up log `server runner limit` after the Docker memory was raised to 10 GiB, 2026-11-26))?
- Which real runs that passed under organization-level registration (the carried-over checks, the 5 jobs from 2 repositories) must be repeated under per-repository registration?
- Can a repository keep idle (pre-started) runners, and how do they count against its maximum and the server's limit?
- How does the scaler order waiting jobs of different repositories when the server is full, and can a repository whose maximum is reached hold a server slot another repository could use?
