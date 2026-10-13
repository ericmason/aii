#!/bin/sh
# Runs inside cline.Dockerfile. Starts one Cline session in a throwaway
# project under $HOME, then resumes it for a second user turn. Session
# files land in $HOME/.cline/data/sessions/.
set -eu

model="${MODEL:-openai/gpt-4.1-mini}"
proj="$HOME/proj"
mkdir -p "$proj"
cd "$proj"

cline -P openrouter -m "$model" --yolo -c "$proj" \
	"Create a file greet.sh that prints 'hello from the fixture', then run it with sh."

sessions="$HOME/.cline/data/sessions"
id=$(ls -t "$sessions" | grep -v '\.' | head -n 1)
echo "resuming session $id"

cline -P openrouter -m "$model" --yolo -c "$proj" --id "$id" \
	"Add a second line to greet.sh that prints 'goodbye', then run it again."

cline --version
