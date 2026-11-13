# OpenClaw for shpyrd: the official image, with the setup screen in front
# of the Control UI. The platform injects PORT and mounts the data volume
# at /data; OpenClaw's state is a directory of its own inside it.

# The setup page: a Next application on the shpyrd design system,
# compiled to static files the Go program embeds.
FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web ./
RUN npm run build

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
COPY --from=web /src/web/out ./web/out
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/openclaw-setup .

# The OpenClaw release. Bumping this tag is how OpenClaw is updated.
FROM ghcr.io/openclaw/openclaw:2026.11.3
COPY --from=build /out/openclaw-setup /usr/local/bin/openclaw-setup
# The platform runs images as a user, never root; node is uid 1000 in the
# image. The volume's root belongs to root, handed to the process's group
# by the platform, so the state lives in a directory of our own in it.
USER 1000:1000
ENV OPENCLAW_STATE_DIR=/data/openclaw OPENCLAW_CONFIG_PATH=/data/openclaw/openclaw.json PORT=8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/openclaw-setup"]
