---
name: commit
description: Use whenever creating a git commit in this repository. Keeps graphify-out/ (the versioned knowledge graph) in sync and committed together with code changes.
---

# Committing in ProxyBridgeService

`graphify-out/` is versioned: the knowledge graph must always match the committed code.

1. If any code files changed, run `graphify update .` first (AST-only, no API cost).
2. Stage the code changes **and** `graphify-out/` together: `git add <changed files> graphify-out/`.
   `.gitignore` already drops graphify's local state (`cache/`, `manifest.json`, `cost.json`,
   `.graphify_root`, `.graphify_python`), so never force-add those.
3. Commit graph and code in the same commit; `git status` must show nothing under `graphify-out/` afterwards.
4. Also commit changes under `.cursor/` and `.claude/` (agent rules and skills) when they are part of the task.
5. Use conventional commit messages (`feat:`, `fix:`, `chore:` …), matching `git log`.
6. Don't push unless asked: a push to `main` with a new `VERSION` triggers a release in CI.

The same rules live in `.cursor/rules/commit.mdc` for Cursor; keep both in sync.
