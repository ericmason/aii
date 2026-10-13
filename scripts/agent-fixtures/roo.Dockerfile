# Runs the Roo Code CLI for a short scripted task so aii's Roo Code
# parser can be checked against real task files. See roo-run.sh.
#
# The Roo Code CLI loads the Roo Code extension bundle under a VS Code
# API shim, so it writes the same tasks/<id>/ layout the extension does,
# rooted at ~/.vscode-mock/global-storage instead of VS Code's
# globalStorage/rooveterinaryinc.roo-cline.
FROM node:22-slim

ARG ROO_VERSION=0.1.17
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates procps \
 && rm -rf /var/lib/apt/lists/* \
 && curl -fsSL https://raw.githubusercontent.com/RooCodeInc/Roo-Code/main/apps/cli/install.sh \
    | ROO_VERSION="${ROO_VERSION}" ROO_INSTALL_DIR=/opt/roo ROO_BIN_DIR=/usr/local/bin sh

COPY roo-entrypoint.sh /usr/local/bin/roo-entrypoint.sh
ENTRYPOINT ["sh", "/usr/local/bin/roo-entrypoint.sh"]
