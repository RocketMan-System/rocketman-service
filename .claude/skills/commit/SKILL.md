---
name: commit
description: Use whenever creating a git commit in this repository. Keeps graphify-out/ (the versioned knowledge graph) in sync and committed together with code changes.
---

# Committing in ProxyBridgeService

`graphify-out/` is versioned: the knowledge graph must always match the committed code.

1. The `pre-commit` hook rebuilds the graph (`graphify update .`) and stages `graphify-out/`
   automatically. If `.git/hooks/pre-commit` is missing, run `scripts/install-git-hooks.sh` first;
   if graphify is unavailable, run `graphify update .` and `git add graphify-out/` by hand.
2. Stage only the code changes yourself; the hook adds `graphify-out/`.
   `.gitignore` already drops graphify's local state (`cache/`, `manifest.json`, `cost.json`,
   `.graphify_root`, `.graphify_python`, dated snapshot folders), so never force-add those.
3. Commit graph and code in the same commit; `git status` must show nothing under `graphify-out/` afterwards.
4. Also commit changes under `.cursor/` and `.claude/` (agent rules and skills) when they are part of the task.
5. Use conventional commit messages (`feat:`, `fix:`, `chore:` …), matching `git log`.
6. Don't push unless asked: a push to `main` with a new `VERSION` triggers a release in CI.

The same rules live in `.cursor/rules/commit.mdc` for Cursor; keep both in sync.
