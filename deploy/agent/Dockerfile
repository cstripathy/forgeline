# Image for pipeline steps that use `agent: {provider: claude-code}`. It holds only the agent's own
# command line; the login is supplied at run time as a secret (CLAUDE_CODE_OAUTH_TOKEN from
# `claude setup-token`, or ANTHROPIC_API_KEY), never baked into the image.
#
#   docker build -t forgeline-agent-claude deploy/agent
FROM node:22-bookworm-slim
ARG CLAUDE_CODE_VERSION=2.1.312
RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" && npm cache clean --force \
 && apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
# The agent runs as an ordinary user; HOME must be writable for its own state.
USER node
ENV HOME=/home/node
