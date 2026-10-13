#!/bin/sh
# Builds and runs the <tool>.Dockerfile image named by this script's
# file name (aider-run.sh runs aider.Dockerfile, and so on). The
# container uses <output-dir> as $HOME, so the agent's session files
# land on the host.
#
# Usage: scripts/agent-fixtures/<tool>-run.sh <output-dir>
#
# The container gets OPENROUTER_API_KEY from this script's environment,
# or from a Docker env file when ENV_FILE names one. MODEL overrides the
# OpenRouter model id (default openai/gpt-4.1-mini). Index the output
# with a throwaway database, never your real one:
#
#   HOME=<output-dir> AII_DB=<output-dir>/aii.db aii index
set -eu

here=$(cd "$(dirname "$0")" && pwd)
tool=$(basename "$0" -run.sh)
out="${1:?usage: $tool-run.sh <output-dir>}"

if [ -n "${ENV_FILE:-}" ]; then
	set -- --env-file "$ENV_FILE"
elif [ -n "${OPENROUTER_API_KEY:-}" ]; then
	set -- -e OPENROUTER_API_KEY
else
	echo "set OPENROUTER_API_KEY or ENV_FILE" >&2
	exit 2
fi

mkdir -p "$out"
out=$(cd "$out" && pwd)
image="aii-fixture-$tool"

# The Roo CLI saves the API key in its mock VS Code secret storage under
# the output directory. Delete it however the run ends.
trap 'rm -f "$out/.vscode-mock/global-storage/secrets.json"' EXIT
trap 'exit 1' HUP INT TERM

docker build -q -t "$image" -f "$here/$tool.Dockerfile" "$here"
docker run --rm "$@" \
	-e MODEL="${MODEL:-openai/gpt-4.1-mini}" \
	-e HOME=/data \
	-v "$out:/data" \
	"$image"
