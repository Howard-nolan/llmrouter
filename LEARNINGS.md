
## 2026-02-11 - PR #1: Scaffold repo: go.mod, README, CI, PR template

**Change Summary:**
Initial project scaffolding for llmrouter. Sets up the Go module, project README, .gitignore, GitHub PR template, and a CI workflow that auto-appends learnings from merged PRs to LEARNINGS.md.

**How It Works:**
-  initializes the Go module (, Go 1.25.2). No dependencies yet — those come when we start building packages.
-  documents the project overview, API surface (unified  endpoint, , , , ), quick start, and build commands.
-  covers Go binaries, ONNX model files, env secrets, IDE files, Python training artifacts, and Claude Code config.
-  defines three sections (Change Summary, How It Works, Additional Notes) that serve as a structured learning record.
-  is a GitHub Actions workflow that fires on PR merge: it parses the PR body sections and appends them to , creating a running log of what was learned each PR.

**Additional Notes:**
- This covers the first part of Week 1 (repo scaffolding). The directory structure (, , etc.), Makefile, and docker-compose are not yet created — those come next as we start implementing packages.
-  and  are gitignored (local development config only).

