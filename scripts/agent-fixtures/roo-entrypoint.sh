#!/bin/sh
# Runs inside roo.Dockerfile. Runs one Roo Code task in a throwaway
# project under $HOME. The task asks for a file write and a command, so
# the saved conversation holds several tool calls and results.
set -eu

model="${MODEL:-openai/gpt-4.1-mini}"
proj="$HOME/proj"
mkdir -p "$proj"

roo --print --provider openrouter -m "$model" -w "$proj" \
	"Create a file greet.sh that prints 'hello from the fixture', then run it with sh and tell me what it printed."

roo --version || true
