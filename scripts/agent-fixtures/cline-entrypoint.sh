#!/bin/sh
# Runs inside cline.Dockerfile. Runs two Cline tasks in a throwaway
# project under $HOME. Session files land in $HOME/.cline/data/sessions/.
#
# The second task is a new session: Cline CLI 3.0.71 resumes a session
# (--id) only in interactive mode, which needs a TTY.
set -eu

model="${MODEL:-openai/gpt-4.1-mini}"
proj="$HOME/proj"
mkdir -p "$proj"
cd "$proj"

cline -P openrouter -m "$model" --yolo -c "$proj" \
	"Create a file greet.sh that prints 'hello from the fixture', then run it with sh."

cline -P openrouter -m "$model" --yolo -c "$proj" \
	"Add a second line to greet.sh that prints 'goodbye', then run it again."

cline --version
