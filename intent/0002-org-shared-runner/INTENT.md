# Intent: Shared in-house runner server

Author: dewevrei. Status: accepted.

## Problem

Developers in the organization want one in-house workflow server that runs our runner program, so that they can deploy their work through it and the organization can use it for all of its repositories. Change 0001 made deploy-job runners scale automatically (confirmed (evidence: `intent/0001-deploy-runner-autoscale/PLAN.md`, M7 results)), but one scaler serves exactly one repository (confirmed (evidence: 0001 SPEC settings table, pools "exactly one" and one repository URL per pool)). A second repository cannot be added; switching to another repository means editing the settings and restarting the scaler, because the settings are read once at startup and there is no reload path (confirmed (evidence: `internal/daemon/startup.go`, `start` calls `LoadConfig` once)). A second scaler cannot run under the same user (confirmed (evidence: 0001 PLAN M7-8, second instance exits 1 with "cannot take the scaler lock")).

## Proposed outcome

- While the runner program runs on the server, jobs that ask for our runner in `runs-on`, from repositories the organization allowed, run on that server (fork pull requests excepted, below).
- When deploy jobs from 5 repositories arrive at the same time, the server creates one single-use runner container per job and runs them (example server: 20 cores, 64 GB; this size is an assumption).
- Jobs beyond what the server can run at once wait in GitHub's queue and start when a runner slot frees up (GitHub cancels a self-hosted job after 24 hours in the queue, confirmed (evidence: https://docs.github.com/en/actions/reference/limits, "Job queue time")).
- When the organization adds a repository, that repository only needs `runs-on` in its workflow; the scaler settings do not change.
- The organization chooses on GitHub which repositories may use the runner, as ARC and runner-up do (confirmed (evidence: runner-up `internal/config/config.go` at 013cc74, "serving every repository the runner group allows")); the scaler keeps no repository or workflow allowlist. This replaces 0001's repository-level registration and its main-branch-only rule.
- Jobs from allowed repositories run on any branch, pull request or manual dispatch, except jobs of pull requests from forks, which fail before their first step. Jobs not on the main branch run but do not get the AWS role, because the trust policy allows only main (confirmed (evidence: 0001 INTENT Problem)).
- The checks carried over from 0001 pass in real runs:
  - after `service stop`, the service stays loaded, and after `launchctl kickstart` the scaler takes jobs again;
  - reinstalling the service while a job runs lets the job finish, and the service comes back loaded;
  - after `service uninstall` during a job, no runner container is left;
  - when the GitHub service stops answering polls, the scaler reports the error within about 6 minutes (5 minutes without an answer plus the uncut poll of about 50 seconds) and takes jobs again after the service recovers;
  - a message that was pending when a poll was cut arrives in a later poll;
  - the number of rate-limit responses to polls during the other real runs is recorded from the scaler logs (no separate long-running test).
- After a host restart, the server takes jobs again within 5 minutes with no human action.
- Out of scope: jobs that use `environment:` (their OIDC subject differs from what the trust policy allows), serving personal-account repositories, fairness between repositories (jobs are taken in the order GitHub offers them), CI-specific labels, caches and sizing (CI jobs that use this label are not refused), moving the team repositories' existing deploy workflows onto this runner, running and testing on Linux, multiple hosts, VM isolation.

## Affected users and systems

- Developers of the organization's repositories and their workflows; organization owners (runner group settings, GitHub App installation and permissions).
- The in-house workflow server. Today it is this Mac (macOS on Apple Silicon, Docker Desktop).
- The GitHub organization (GitHub Free plan, unverified (stated by the user)): its runner groups, the GitHub App's permissions and installation, and organization-level runner registration.
- OIDC, the AWS role, the registry and the Kubernetes cluster: this change does not modify them, but jobs reach them.

## Constraints

- Register runners at the organization level; the organization's runner group settings decide which repositories may use them. Reason: adding a repository then needs no scaler change; ARC registers the same way (confirmed (evidence: ARC `charts/gha-runner-scale-set/values.yaml`, `githubConfigUrl` examples include `https://github.com/myorg`)), and Free-plan organizations can use runner groups (confirmed (evidence: https://github.blog/changelog/2024-10-17-actions-runner-groups-now-available-for-organizations-on-free-plan, "organizations on all plans, including the Free plan, can now utilize GitHub Actions runner groups"; github/docs commit 506fa5558 of the same day lists Free-plan organizations among those that "can create and manage additional runner groups", while the rendered page body still names only the Team plan)).
- Public repositories of the organization may use the runner, so the organization's runner group must allow public repositories, which it does not by default (confirmed (evidence: https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/manage-access, "By default, only private repositories can access runners in a runner group, but you can override this")). Reason: the organization wants all of its repositories served.
- Refuse jobs of pull requests from forks. Reason: public repositories accept pull requests from anyone, and code from outside the organization must not reach the internal network or the runner's credentials; runner-up does the same by default (confirmed (evidence: runner-up `internal/config/config.go` at 013cc74, `AllowForkPullRequests` "Off by default")).
- The runner label stays `linux-arm64-deploy` and is meant for deploy jobs; nothing refuses other jobs that use it. Reason: CI goes to its own label later, as decided in 0001 (confirmed (evidence: 0001 INTENT Constraints)).
- One concurrency limit covers the whole server. Reason: all repositories share one server.
- Personal-account repositories must be addable later without redesigning the scaler. Reason: developers may want to use the server for their own repositories, and a personal account has no organization-level runners, so each such repository needs its own registration (unverified (runner-up `internal/config/config.go` says "A personal account has no organization-wide runners"; no official GitHub statement found)).
- Linux support must be addable later without redesigning the scaler. Reason: the real server will run Linux.
- Build and test on macOS (launchd, Docker Desktop). Reason: this Mac is the always-on host on the allowed network that runs the scaler today (confirmed (evidence: 0001 INTENT Affected users and systems)).
- AWS access stays with the administrator's OIDC trust policy; the scaler injects no credentials and does not know which repositories may assume the role. Reason: the trust policy lists repositories one by one and adding one is the administrator's process (confirmed (evidence: 0001 INTENT Problem, "the role allows only the main branch of the registered team repositories")).
- A job may read its own runner's credentials and JIT configuration; this stays an accepted risk although jobs now run on any branch. Reason: GitHub treats one job as one trust boundary (confirmed (evidence: 0001 SPEC Areas of concern 18)), each runner takes one job and is removed, only people with write access to an allowed repository can start its non-fork jobs, and fork pull requests are refused.
- Keep all of 0001's isolation guarantees: non-root runner, no host Docker socket, no privileged containers, per-job CPU and memory limits, no path for job code to control host Docker or other runners, builds in a per-runner rootless build container. Reason: jobs from many people share one server and can reach the internal network.
- Do not bypass the administrator's access restrictions, and do not run runners inside the cluster. Reason: administrator policy, and the team cannot create cluster roles or CRDs (confirmed (evidence: 0001 INTENT Constraints)).
- Keep Go, `github.com/actions/scaleset`, and the GitHub App owned by a personal account. Reason: the library is GitHub's official one and ARC uses it; the App keeps token renewal in the library and separate from personal tokens (confirmed (evidence: 0001 INTENT Constraints)).
- Run the reboot test last, after every other outcome works. Reason: it verifies the finished behavior and interrupts the host and any running test.
- No internal names and no secret values in the repository. Reason: the repository is public.

## Open questions

- Is the server-wide limit a number of runners or a CPU and memory total? The current per-job limits (runner 1 CPU / 1 GB plus build 2 CPU / 2 GB) for 5 jobs sum to 15 CPU / 15 GB, above this Mac's Docker VM (10 CPU / 7.75 GiB, confirmed (evidence: 0001 INTENT Constraints)). Which defaults change, and how is the 5-repository outcome verified on this Mac?
- The GitHub App needs the organization permission "Self-hosted runners: Read and write" for organization-level runners (confirmed (evidence: https://docs.github.com/en/actions/tutorials/use-actions-runner-controller/authenticate-to-the-api)). Who adds it and installs the App on the organization, and is that permission enough for our library calls?
- What happens to 0001's settings format (one repository URL and workflow allowlist per pool)? 0001's repository-level scale set was already deleted (confirmed (evidence: 0001 PLAN M7-9)).
- 0001 left five items to a later change (confirmed (evidence: 0001 SPEC "다음 INTENT로 넘긴 것")). Changing existing workflows' `runs-on` and running the first `kubectl apply -f` from a main workflow belong to the out-of-scope workflow migration; the reusable-workflow allow rule no longer applies without an allowlist. `environment:` jobs are out of scope. Measuring team build memory is not needed: per-job CPU and memory stay settings with defaults that are raised when a build needs more.
- Does the fork refusal also cover `pull_request_target` and `workflow_run` runs triggered by fork pull requests?
