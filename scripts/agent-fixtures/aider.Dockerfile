# Runs Aider for a short scripted chat so aii's Aider parser can be
# checked against real session files. See aider-run.sh.
FROM python:3.12-slim

ARG AIDER_VERSION=0.86.2
RUN apt-get update \
 && apt-get install -y --no-install-recommends git \
 && rm -rf /var/lib/apt/lists/* \
 && pip install --no-cache-dir "aider-chat==${AIDER_VERSION}"

COPY aider-entrypoint.sh /usr/local/bin/aider-entrypoint.sh
ENTRYPOINT ["sh", "/usr/local/bin/aider-entrypoint.sh"]
