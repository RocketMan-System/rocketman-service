#!/bin/sh
# Sets up git hooks so graphify-out/ is always committed together with code:
#   pre-commit    rebuilds the graph and stages graphify-out/ (scripts/git-hooks/pre-commit)
#   post-checkout graphify's rebuild after branch switches
#   merge driver  graphify's union merge for graphify-out/graph.json
# graphify's post-commit hook is removed: it rebuilds after the commit and
# would leave graphify-out/ modified instead of committed.
set -e

cd "$(git rev-parse --show-toplevel)"
HOOKS_DIR=$(git rev-parse --git-path hooks)

if ! command -v graphify >/dev/null 2>&1; then
    echo "graphify is not installed (uv tool install graphifyy)" >&2
    exit 1
fi

graphify hook install
rm -f "$HOOKS_DIR/post-commit"

cp scripts/git-hooks/pre-commit "$HOOKS_DIR/pre-commit"
chmod +x "$HOOKS_DIR/pre-commit"

echo "Git hooks installed: pre-commit (graphify), post-checkout (graphify), graph.json merge driver"
