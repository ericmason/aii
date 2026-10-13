#!/bin/sh
# Runs inside aider.Dockerfile. Creates a throwaway git repo under $HOME
# and runs two one-message Aider sessions against it, so the project's
# .aider.chat.history.md holds two "# aider chat started at" sections.
set -eu

model="${MODEL:-openai/gpt-4.1-mini}"
proj="$HOME/proj"
mkdir -p "$proj"
cd "$proj"

git init -q
git config user.email fixture@example.com
git config user.name "Fixture Runner"
printf 'def add(a, b):\n    return a + b\n' > calc.py
git add calc.py
git commit -qm "Initial calc module"

run_aider() {
	aider --model "openrouter/$model" \
		--yes-always --no-auto-commits --no-check-update \
		--analytics-disable --no-show-release-notes --no-pretty --no-stream \
		--message "$1" calc.py
}

run_aider "Add a subtract(a, b) function to calc.py."
run_aider "Add a one-line docstring to every function in calc.py."

aider --version
