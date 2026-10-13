# Runs the Cline CLI for a short scripted task so aii's Cline parser can
# be checked against real session files. See cline-run.sh.
FROM node:22-slim

ARG CLINE_VERSION=3.0.71
# The CLI loads CA certificates from /etc/ssl/certs, which node:22-slim
# doesn't ship.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN npm install -g "cline@${CLINE_VERSION}" && npm cache clean --force

COPY cline-entrypoint.sh /usr/local/bin/cline-entrypoint.sh
ENTRYPOINT ["sh", "/usr/local/bin/cline-entrypoint.sh"]
