# Claude Code in a container: it can edit the workspace you give it with -w,
# but nothing else on your machine.
#
#   aic build -t claude-code examples/claude-code
#   aic run -it -w . --network=host -e ANTHROPIC_API_KEY claude-code
FROM node:22-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ripgrep ca-certificates \
 && rm -rf /var/lib/apt/lists/*

ARG CLAUDE_CODE_VERSION=latest
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
 && npm cache clean --force

# Claude Code won't skip permission prompts as root.
RUN useradd -m -s /bin/bash claude
USER claude
ENV USE_BUILTIN_RIPGREP=0
WORKDIR /workspace
ENTRYPOINT ["claude"]
