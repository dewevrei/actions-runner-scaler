# Intent: Easier setup and personal access tokens

Author: dewevrei. Status: accepted.

## Problem

Someone who clones this repository and wants to run the scaler for their own repositories has to do every setup step by hand: create a GitHub App with the right permissions, copy the example settings file and fill in every value, and find out what is wrong only when the scaler fails to start (confirmed (evidence: `README.md` sections "GitHub setup", "Build" and "Configuration"; `config.example.toml`; the scaler has no command that writes or checks the settings before `run`)). The only supported credential is a GitHub App (confirmed (evidence: `README.md` "Configuration" table, `github.app-client-id` and `github.app-private-key-file` required)), although GitHub's library also accepts a personal access token (confirmed (evidence: `github.com/actions/scaleset@v0.5.0` `client.go` line 230, `NewClientWithPersonalAccessToken`; its `README.md` line 151)).

A first-time user should reach a running scaler without editing the settings file by hand, and every command that works with a GitHub App should also work with a personal access token.

Other self-hosted runner scalers make the first setup shorter: runner-up writes its settings with an `init` command (confirmed (evidence: runner-up `README.md` line 102 at 013cc74)), runscaler and runpool check the host and credentials with a `doctor` command before serving (confirmed (evidence: runscaler `README.md` line 167 at 6f33444; runpool `README.md` lines 118 and 125 at f1a2ab3)), ARC gives a GitHub App creation link with the permissions filled in (confirmed (evidence: ARC `docs/authenticating-to-the-github-api.md` lines 51–63 at eca1a62)), and runscaler, runpool and ARC each accept a personal access token (confirmed (evidence: runscaler `docs/configuration.md` lines 15–24; runpool `docs/deployment.md` lines 163–172; ARC `charts/gha-runner-scale-set/values.yaml` lines 14–44)).

## Proposed outcome

- An `init` command writes the settings file from command-line flags (for example the repositories and each one's maximum number of runners) and asks in the terminal only for required values that were not given.
- Commands add a repository to an existing settings file and remove one from it, so that the file need not be edited by hand.
- A `doctor` command checks, whether or not the scaler is running, the settings, the key or token files, each repository's GitHub App installation or token permissions, Docker, and the runner images, and prints for each check whether it passed and, if not, why and how to fix it.
- The scaler prints a GitHub App creation link with the needed permissions already filled in, so that creating the App is opening the link and confirming.
- The scaler can use a personal access token instead of a GitHub App.
- A repository entry can name its own token, so that repositories of different owners can be served with different tokens on one server; entries that name none use the server's default credential (a GitHub App or one token).
- Out of scope: an install script, release binaries, publishing the runner image to a public registry, the GitHub App Manifest flow, a web setup wizard, automatic fixes by `doctor`, and reloading the settings without a restart.

## Affected users and systems

- People who run the scaler for their own repositories, including people outside the team who clone this public repository.
- The current server on this Mac, whose settings file and installed service keep working unchanged.
- GitHub: GitHub Apps and personal access tokens, and the repositories' runner registration.
- Docker on the host: `doctor` reads its state and the local images.

## Constraints

- The command that checks the setup is named `doctor`, and there is no separate `validate` command. Reason: the user wants one check command; runscaler and runpool name their host-and-credential check `doctor` (confirmed (evidence: runscaler `README.md` line 167; runpool `README.md` line 125)) and keep a separate validate command (confirmed (evidence: runscaler `README.md` line 165; runpool `README.md` line 93)), which this change folds into `doctor`.
- `doctor` creates, changes and deletes nothing on GitHub, in Docker, or in files. Reason: a check must be safe to run at any time, including while the service runs.
- `init` and the add and remove commands change only the settings file. Reason: registration, the service and Docker stay under the existing commands.
- Secrets are referenced by file path only, and a token file must have the same restricted file mode as the App key; no command prints, logs or writes a key or token value. Reason: the settings already reference secrets by path only (confirmed (evidence: `config.example.toml` line 3; `README.md` "Configuration" table, `github.app-private-key-file` mode `0600` or `0400`)).
- The GitHub App stays the recommended credential, and the documentation says why. Reason: GitHub's library prefers it and calls a token "simpler but broader scoped" (confirmed (evidence: `github.com/actions/scaleset@v0.5.0` `README.md` lines 147–151 and 349)).
- The App creation link fills in the permissions in the URL, as ARC does, and does not use the Manifest flow. Reason: none of ARC, runner-up, runscaler, runpool and efr uses the Manifest flow, and ARC's link is the only App creation link among them (confirmed (evidence: text search of the five clones for "app manifest" and `/settings/apps/new`, which matches only ARC `docs/authenticating-to-the-github-api.md` lines 51–63)).
- Keep 0002's scaling decisions: repository targets only, one scale set and one maximum per repository, the server limit from CPU and memory, scale sets deleted only by the delete command. This change relaxes only 0002's constraint that the GitHub App is the only credential; a repository served by a token needs the matching token permission instead of the App's "Administration: Read and write". Reason: this change adds ways to set up and authenticate, not new scaling behavior.
- Existing settings files that use a GitHub App keep working without edits. Reason: the running service on this Mac uses one.
- Adding or removing a repository still needs a scaler restart. Reason: the settings are read once at start-up and reloading is out of scope (confirmed (evidence: 0002 INTENT Constraints)).
- Keep Go and `github.com/actions/scaleset`; build and test on macOS. Reason: as decided in 0001 and 0002.
- No internal names and no secret values in the repository. Reason: the repository is public.

## Open questions

- Which kinds of personal access token (classic, fine-grained) can register repository-level scale sets, with which scopes or permissions, and can a fine-grained token cover repositories of more than one owner (unverified)?
- Can `doctor` confirm a token's or App's permission to register runners on a repository without creating anything?
- Which values does `init` ask for when no runner image has been built yet, and does it fill in the image values of a locally built image?
- Does the App creation link work for both a personal account and an organization as the App's owner, and which permissions does it fill in?
- Do the add and remove commands keep the comments and order of a hand-edited settings file?
- What does `init` do when a settings file already exists at its target path?
- Does `init` check that the key or token file it is given exists and has the restricted file mode?
- How are REST rate-limit waits shared between repositories that use the same token, given that 0002 shares them per App installation?
- What does `doctor` print for a check it cannot run (for example Docker not running), and does it then run the other checks?
