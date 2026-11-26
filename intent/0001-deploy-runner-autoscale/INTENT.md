# Intent: Automatic scaling of self-hosted runners for deploy jobs

Author: dewevrei. Status: implemented.

Whether the scaler runs again within 5 minutes after a host restart is checked in 0002 (`intent/0002-org-shared-runner/`).

## Problem

The team's deploy jobs must connect to the cluster and push images, which administrator policy allows only from the allowed network (confirmed (evidence: test)). For AWS credentials, a deploy job receives a role through GitHub OIDC without keys, and the role allows only the main branch of the four registered team repositories (confirmed (evidence: administrator notice, 2026-11-24)). Nothing on the hosts of that network starts runners automatically, so team members have to start them by hand.

## Proposed outcome

- When a deploy job arrives, a runner container takes it within 1 minute and is removed after finishing that one job.
- Concurrent runners never exceed the configured maximum, and when there are no jobs only the configured number of idle runners remain.
- A job on the test repository's main branch receives the role through OIDC and lists the cluster's Pods successfully.
- An image can be built and pushed inside a main-branch deploy job.
- Only main-branch jobs of the repositories and workflows listed in the settings run; all others fail before their first step.
- After a host restart or an abnormal exit, the scaler runs again within 5 minutes with no human action.
- The operator can see the status (number of runners, recent errors) and stop the scaler.
- The number of runners, CPU and memory, and the allowlist can be changed per pool, and pools can be added later.
- Out of scope: moving the deploy workflows, the other three repositories, a CI pool (all for the next INTENT).
- Out of scope: organization-level registration, multiple hosts, VM isolation.

## Affected users and systems

- The team's deploy workflows and team members; this time, one test repository.
- The always-on host on the allowed network (macOS on Apple Silicon, Docker Desktop).
- GitHub scale sets, the GitHub App, OIDC, the AWS role, the registry, the Kubernetes cluster.

## Constraints

- Do not bypass the administrator's access restrictions. Reason: administrator policy.
- Runners inside the cluster (ARC and similar) cannot be used. Reason: the team's permissions cannot create roles or CRDs (confirmed).
- Use Go and `github.com/actions/scaleset`. Reason: it is GitHub's official library and ARC uses it.
- Authenticate with a GitHub App owned by a personal account, register at repository level, and start with the test repository. Reason: the library handles token renewal, and authentication stays separate from personal tokens.
- The runner name and `runs-on` label are `linux-arm64-deploy`, with no extra labels. Reason: the OS-architecture-purpose convention.
- The defaults per pool are a maximum of 2, 0 idle, runner CPU 1 and memory 1 GB, build container CPU 2 and memory 2 GB; all are configurable and the code sets no upper limit (decided 2026-11-24 by the user). Reason: the total fits in the Docker VM (10 CPU, 7.75 GiB) (confirmed).
- Runners run as non-root, without the host Docker socket, and with CPU and memory limits. No container uses privileged mode, and job code gets no way to control host Docker or other runners. Reason: job code reaches AWS credentials, and the App key is on the same host.
- Builds run in a rootless BuildKit container that the scaler starts and removes together with each runner; the job only sends build requests (decided 2026-11-24 by the user).
- `seccomp=unconfined` is allowed only for the build container, and is replaced once a custom seccomp profile works (decided 2026-11-24 by the user). Reason: rootless BuildKit needs it (confirmed (evidence: local test)).
- The scaler injects no credentials, and the administrator's trust policy enforces the main-only restriction on AWS access (decided 2026-11-24 by the user). Reason: the administrator issues no keys.
- Image build and push in deploy jobs are in scope (decided 2026-11-24 by the user). Reason: pushes work only from the allowed network (unverified).
- CI goes later to a separate pool with its own label, settings and cache, and is not mixed in (decided 2026-11-24 by the user). Reason: keep the main-only guarantee.
- This time, register one CD pool on the test repository only; writes to its main branch need approval, and the user installs the App before the test (decided 2026-11-24 by the user). Reason: keep it separate from other team members' work.
- After a reboot the host logs in and starts Docker with no human action (confirmed; sleep and power-loss recovery unverified). Reason: the premise of unattended operation.
- The repository is public and holds no license, internal names, or secret values. Commits, pushes, GitHub settings changes and real tests happen only after user approval.

## Open questions

- None.
