# Commands
- Build: `go build ./...`. Test: `go test -race ./...`.
- Tests with the `integration` build tag call real GitHub and Docker; run them only after user approval.

# Conventions
- Write design docs at the repo root one at a time, in order: `INTENT.md` → `SPEC.md` → `PLAN.md`. Start the next doc only after the previous one is `Status: accepted`. Read `SPEC.md` and `PLAN.md` before changing behavior.
- Commit without asking once `gofmt -l .` prints nothing, `go vet ./...` and `go test -race ./...` pass, and a fresh-context review has no required findings; afterwards report the hash, message, and `git diff --stat`. Push committed work to `origin` without asking and report what was pushed. Before any PR, show `git diff --stat` and the message and get user approval. Only the user merges PRs.
- Go code follows Effective Go (https://go.dev/doc/effective_go) and Go Code Review Comments (https://go.dev/wiki/CodeReviewComments).
- Inject dependencies through constructors; no global state. Define small interfaces in the consuming package.

# Gotchas
- Never read, print, or write AWS keys, GitHub tokens, or the GitHub App private key to files, logs, or commits (the user registers them). Do not write internal team names (cluster, IAM policy, hostname); write account numbers as `<acct>`.
- Do not bypass the admin's access restrictions (IAM policy, etc.). Use only paths that work normally from a host on the allowed network.
- Do not change GitHub settings (Environment, secrets, variables, runner or scale set registration), create remote repos, run `kubectl apply/delete/patch/scale`, or change permissions without user approval. Delete test resources right after a test and report it.
- macOS (Apple Silicon) default bash is 3.2 (no associative arrays); zsh does not word-split variables.
- Docker Desktop containers are linux/arm64; the runner image and tools must be arm64.
