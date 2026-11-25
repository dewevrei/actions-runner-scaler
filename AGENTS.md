# Commands
- Build: `go build ./...`. Test: `go test -race ./...`.
- Tests with the `integration` build tag call real GitHub and Docker; delete what they create and report it.

# Conventions
- Each change gets its own directory `intent/NNNN-<kebab-name>/` (number never reused) holding `INTENT.md` → `SPEC.md` → `PLAN.md`, written one at a time in that order; start the next doc only after the previous one is `Status: accepted` (or `implemented`). Start a new change only when all three docs of the highest-numbered change are accepted or implemented. R numbers continue across changes. Before changing behavior, read the highest-numbered change's `SPEC.md` and `PLAN.md` and the changes it `Extends`. In code comments, an unqualified SPEC/PLAN reference means 0001; cite later changes with their number (e.g. `0002 PLAN M1`).
- Commit without asking once `gofmt -l .` prints nothing, `go vet ./...` and `go test -race ./...` pass, and a fresh-context review has no required findings; afterwards report the hash, message, and `git diff --stat`. Push committed work to `origin` without asking and report what was pushed. Before any PR, show `git diff --stat` and the message and get user approval. Only the user merges PRs.
- Go code follows Effective Go (https://go.dev/doc/effective_go) and Go Code Review Comments (https://go.dev/wiki/CodeReviewComments).
- Inject dependencies through constructors; no global state. Define small interfaces in the consuming package.

# Gotchas
- Never read, print, or write AWS keys, GitHub tokens, or the GitHub App private key to files, logs, or commits (the user registers them). Do not write internal team names (cluster, IAM policy, hostname); write account numbers as `<acct>`.
- Do not bypass the admin's access restrictions (IAM policy, etc.). Use only paths that work normally from a host on the allowed network.
- Get user approval before any PR or cluster change (kubectl/helm writes). Delete test resources right after a test and report it.
- macOS (Apple Silicon) default bash is 3.2 (no associative arrays); zsh does not word-split variables.
- Docker Desktop containers are linux/arm64; the runner image and tools must be arm64.
