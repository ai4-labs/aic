# Claude Code in a container: it can edit the workspace you give it with -w,
# but nothing else on your machine.
#
#   aic build -t claude-code examples/claude-code
#   aic run -it -w . --copy-back --network=host -e ANTHROPIC_API_KEY claude-code
FROM node:22-alpine

# Shells and the everyday tools Claude Code reaches for. Alpine's busybox
# provides /bin/sh; bash and the GNU tools behave like on most machines.
# Claude Code's bundled ripgrep is built for glibc, so use Alpine's.
RUN apk add --no-cache \
      bash coreutils findutils grep sed gawk diffutils patch less procps \
      curl ca-certificates openssh-client git jq ripgrep \
      tar gzip xz unzip make
ENV SHELL=/bin/bash USE_BUILTIN_RIPGREP=0

ARG CLAUDE_CODE_VERSION=latest
# Keep npm quiet: no update notice, funding or audit messages; the
# download cache is deleted instead of "npm cache clean --force".
RUN npm install -g --no-update-notifier --no-fund --no-audit --loglevel=error \
      @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
 && rm -rf /root/.npm

# Claude Code won't skip permission prompts as root.
RUN adduser -D -s /bin/bash claude
USER claude
WORKDIR /workspace
ENTRYPOINT ["claude"]
