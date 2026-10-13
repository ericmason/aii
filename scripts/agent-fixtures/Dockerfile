# Image with the pi and opencode CLIs installed, used by run.sh to record
# real session files for aii's parsers. No credentials are baked in: the
# OpenRouter key is passed at `docker run` time.
FROM node:22-slim

ARG PI_VERSION=latest
ARG OPENCODE_VERSION=latest

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git ripgrep \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g "@earendil-works/pi-coding-agent@${PI_VERSION}" "opencode-ai@${OPENCODE_VERSION}" \
 && npm cache clean --force

WORKDIR /work
