# syntax=docker/dockerfile:1
FROM golang:1.27-bookworm AS daemon
WORKDIR /src
COPY daemon/go.mod daemon/go.sum ./
RUN go mod download
COPY daemon/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bearbotd ./cmd/bearbotd

# The root package.json "allowScripts" denies the install scripts of better-sqlite3 (prebuilt binary) and esbuild,
# so npm ci runs no node-gyp and this stage needs no python3 or build-essential.
FROM node:24-bookworm-slim AS node-build
WORKDIR /src
COPY package.json package-lock.json ./
COPY gateway/package.json gateway/
RUN npm ci
COPY gateway/ gateway/
RUN npm run build -w gateway \
 && npm prune --omit=dev

FROM ghcr.io/astral-sh/uv:0.13.6 AS uv

FROM node:24-bookworm-slim
ARG PI_VERSION=1.4.0
ARG TYPESCRIPT_VERSION=7.0.2
ARG PYTHON_VERSION=3.14.10
ARG BEARBOT_UID=1000
ARG BEARBOT_GID=1000
ARG BEARBOT_GW_UID=1001

# build-essential stays: agents compile native npm modules in their extension directories.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bash ca-certificates curl git jq ripgrep fd-find procps tini build-essential \
 && rm -rf /var/lib/apt/lists/* \
 && ln -s /usr/bin/fdfind /usr/local/bin/fd

COPY --from=uv /uv /uvx /usr/local/bin/
ENV UV_PYTHON_INSTALL_DIR=/opt/bearbot/python
RUN uv python install ${PYTHON_VERSION} \
 && ln -s "$(uv python find --managed-python ${PYTHON_VERSION})" /usr/local/bin/python3.14

RUN npm install -g @earendil-works/pi-coding-agent@${PI_VERSION}
RUN npm install --prefix /opt/bearbot/toolchain typescript@${TYPESCRIPT_VERSION} @types/node@24

COPY --from=node-build /src/node_modules /opt/bearbot/gateway/node_modules
COPY --from=node-build /src/gateway/dist /opt/bearbot/gateway/dist
COPY --from=node-build /src/gateway/package.json /opt/bearbot/gateway/package.json
COPY --from=node-build /src/gateway/drizzle /opt/bearbot/gateway/drizzle
COPY extensions/ /opt/bearbot/extensions/
COPY sdk/ /opt/bearbot/sdk/
COPY skills/ /opt/bearbot/skills/
COPY recovery-profile/ /opt/bearbot/recovery-profile/
COPY --from=daemon /out/bearbotd /usr/local/bin/bearbotd

# The node base image owns uid/gid 1000 as user and group "node"; userdel removes both.
RUN userdel --remove node \
 && groupadd -g ${BEARBOT_GID} bearbot \
 && useradd -m -u ${BEARBOT_UID} -g bearbot -s /bin/bash bearbot \
 && useradd -M -u ${BEARBOT_GW_UID} -U -G bearbot -s /usr/sbin/nologin bearbot-gw \
 && mkdir -p /bearbot/profile /bearbot/workspace /bearbot/config \
 && chmod 0700 /bearbot/config \
 && chown -R root:root /opt/bearbot && chmod -R go-w /opt/bearbot

ENV PI_CODING_AGENT_DIR=/bearbot/profile/agent \
    PI_SKIP_VERSION_CHECK=1

VOLUME ["/bearbot/profile", "/bearbot/workspace", "/bearbot/config"]
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/bearbotd"]
CMD ["run", "--config", "/bearbot/config/bearbot.toml"]
