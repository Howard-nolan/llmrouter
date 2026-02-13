
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

