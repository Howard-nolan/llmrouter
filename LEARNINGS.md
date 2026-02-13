
## 2026-02-11 - PR #1: Scaffold repo: go.mod, README, CI, PR template

**Change Summary:**
Initial project scaffolding for llmrouter. Sets up the Go module, project README, .gitignore, GitHub PR template, and a CI workflow that auto-appends learnings from merged PRs to LEARNINGS.md.

**How It Works:**
- `go.mod` initializes the Go module (`github.com/howard-nolan/llmrouter`, Go 1.25.2). No dependencies yet — those come when we start building packages.
- `README.md` documents the project overview, API surface (unified `/v1/chat/completions` endpoint, `/health`, `/metrics`, `/cache/stats`, `/cache/flush`), quick start, and build commands.
- `.gitignore` covers Go binaries, ONNX model files, env secrets, IDE files, Python training artifacts, and Claude Code config.
- `.github/pull_request_template.md` defines three sections (Change Summary, How It Works, Additional Notes) that serve as a structured learning record.
- `.github/workflows/append-learnings.yml` is a GitHub Actions workflow that fires on PR merge: it parses the PR body sections and appends them to `LEARNINGS.md`, creating a running log of what was learned each PR.

**Additional Notes:**
- This covers the first part of Week 1 (repo scaffolding). The directory structure (`cmd/`, `internal/`, etc.), Makefile, and docker-compose are not yet created — those come next as we start implementing packages.
- `CLAUDE.md` and `.claude/` are gitignored (local development config only).


## 2026-02-13 - PR #2: Fix append-learnings workflow stripping inline code

**Change Summary:**
Fixes a bug where the `append-learnings.yml` workflow was stripping all backtick-wrapped inline code (e.g., `go.mod`, `README.md`, endpoint paths) from PR bodies when appending to `LEARNINGS.md`. Also manually restores the PR #1 learnings entry that was corrupted by this bug.

**How It Works:**
- The root cause was direct `${{ }}` interpolation of the PR body into a bash `run:` block. Bash interprets backticks as command substitution, so `` `go.mod` `` became an attempt to execute `go.mod` as a shell command — which fails silently and produces empty output.
- The fix passes the PR body through an `env:` variable instead. GitHub Actions sets env vars in the process environment without shell interpretation, so `"$ENTRY"` is expanded as a plain string with backticks preserved.
- `LEARNINGS.md` is updated to restore all the inline code that was stripped from the PR #1 entry.

**Additional Notes:**
- This is a common GitHub Actions security/correctness gotcha — direct `${{ }}` interpolation in `run:` blocks is also a shell injection vector (e.g., a malicious PR title could execute arbitrary commands). Using `env:` is the recommended safe pattern.
- This covers a fix discovered during Week 1. No new features; purely a bug fix to existing CI infrastructure.

