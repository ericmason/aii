#!/usr/bin/env bash
# Record fresh pi and opencode sessions with the newest releases, so aii's
# parsers can be checked against the current upstream session formats.
#
# Each agent runs a two-turn chat in a throwaway project that makes it
# read a file and write another one (at least one tool call per turn).
# The container's HOME is a host directory, so the session files land on
# the host:
#
#   $OUT/pi/home/.pi/agent/sessions/...
#   $OUT/opencode/home/.local/share/opencode/opencode.db
#
# Usage:
#   OPENROUTER_API_KEY=... scripts/agent-fixtures/run.sh [pi|opencode|all]
#   OPENROUTER_ENV_FILE=path/to/file.env scripts/agent-fixtures/run.sh
#
# Settings (environment):
#   OUT               output root (default ~/scratch/aii-agent-runs)
#   MODEL             OpenRouter model id (default google/gemini-2.5-flash)
#   PI_VERSION        npm version of @earendil-works/pi-coding-agent (default latest)
#   OPENCODE_VERSION  npm version of opencode-ai (default latest)
#
# Gemini CLI is not included: it only speaks Google's Gemini API, and
# OpenRouter offers an OpenAI-compatible API. Copilot Chat runs inside
# VS Code and cannot run headless.

set -euo pipefail

die() { echo "error: $*" >&2; exit 1; }

which="${1:-all}"
here="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-$HOME/scratch/aii-agent-runs}"
MODEL="${MODEL:-google/gemini-2.5-flash}"
image="aii-agent-fixtures"

# The key is passed to docker by name or by env file, never on the
# command line.
if [[ -n "${OPENROUTER_ENV_FILE:-}" ]]; then
    [[ -r "$OPENROUTER_ENV_FILE" ]] || die "cannot read $OPENROUTER_ENV_FILE"
    key_args=(--env-file "$OPENROUTER_ENV_FILE")
elif [[ -n "${OPENROUTER_API_KEY:-}" ]]; then
    key_args=(-e OPENROUTER_API_KEY)
else
    die "set OPENROUTER_API_KEY or OPENROUTER_ENV_FILE"
fi

docker build -q -t "$image" \
    --build-arg "PI_VERSION=${PI_VERSION:-latest}" \
    --build-arg "OPENCODE_VERSION=${OPENCODE_VERSION:-latest}" \
    "$here" >/dev/null

turn1="Read README.md in this directory and summarize it in two sentences."
turn2="Now create notes.txt in this directory containing three short bullet points based on that summary."

# prepare <agent>: fresh HOME and project dirs for one agent.
prepare() {
    local dir="$OUT/$1"
    rm -rf "$dir"
    mkdir -p "$dir/home" "$dir/project"
    cat >"$dir/project/README.md" <<'EOF'
# Lantern

Lantern is a tiny command-line tool that converts Markdown checklists into
a progress report. It reads every `- [ ]` and `- [x]` item in the files you
pass it and prints how many are done, grouped by heading.

Run it with `lantern notes/*.md`.
EOF
}

# run_in <agent> <cmd...>: run a command in the container for one agent.
run_in() {
    local dir="$OUT/$1"
    shift
    docker run --rm \
        --user "$(id -u):$(id -g)" \
        -e HOME=/home/agent \
        "${key_args[@]}" \
        -v "$dir/home:/home/agent" \
        -v "$dir/project:/work/lantern" \
        -w /work/lantern \
        "$image" "$@"
}

run_pi() {
    prepare pi
    run_in pi pi --version
    run_in pi pi --provider openrouter --model "$MODEL" -p "$turn1"
    run_in pi pi --provider openrouter --model "$MODEL" -c -p "$turn2"
}

run_opencode() {
    prepare opencode
    run_in opencode opencode --version
    run_in opencode opencode run --auto -m "openrouter/$MODEL" "$turn1"
    run_in opencode opencode run --auto -m "openrouter/$MODEL" -c "$turn2"
}

case "$which" in
    pi) run_pi ;;
    opencode) run_opencode ;;
    all) run_pi; run_opencode ;;
    *) die "unknown agent: $which (want pi, opencode, or all)" ;;
esac

echo "sessions written under $OUT"
