# syntax=docker/dockerfile:1
#
# Images from one build:
#   docker build -t ibs .                              IBS built in, for local development and the test
#                                                      suites only (not published)
#   docker build -t ibs-launcher --target launcher .   published: the IBS launcher, which downloads and
#                                                      runs signed IBS versions kept in /data
#   docker build -t ibs-relay --target relay .         published: relay only (no UI, no storage code)
# The UI and Go binaries are built on the build machine's own platform and
# cross-compiled for the target (e.g. --platform linux/amd64), so building
# images for another architecture needs no emulation.
FROM --platform=$BUILDPLATFORM node:20-alpine AS ui
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH
# The relay and the launcher have their own versions, in their source.
RUN go build -trimpath -ldflags="-s -w" -o /out/ibs-relay ./cmd/ibs-relay
RUN go build -trimpath -ldflags="-s -w" -o /out/ibs-launcher ./cmd/ibs-launcher
COPY --from=ui /src/web/dist ./web/dist
RUN go build -trimpath -ldflags="-s -w -X ibs/internal/version.Version=$VERSION" -o /out/ibs ./cmd/ibs

FROM alpine:3.20 AS base
RUN apk add --no-cache ca-certificates su-exec && adduser -D -u 10001 ibs && mkdir /data /join && chown ibs /data /join
COPY docker-entrypoint.sh /usr/local/bin/
COPY LICENSE /usr/share/licenses/ibs/LICENSE
LABEL org.opencontainers.image.source="https://github.com/spagettikod/ibs" \
      org.opencontainers.image.licenses="MIT"
VOLUME /data
# Runs as the unprivileged "ibs" user (see docker-entrypoint.sh).
ENTRYPOINT ["docker-entrypoint.sh"]

FROM base AS relay
COPY --from=build /out/ibs-relay /usr/local/bin/ibs-relay
ENV IBS_BIN=ibs-relay
# 7444 TCP (relayed connections) and UDP (address discovery)
EXPOSE 7444/tcp 7444/udp

FROM base AS launcher
COPY --from=build /out/ibs-launcher /usr/local/bin/ibs-launcher
ENV IBS_BIN=ibs-launcher
# 8080 admin UI, 9000 S3, 7443 site-to-site (TCP, and UDP for direct QUIC), 7480 storage node
EXPOSE 8080 9000 7443/tcp 7443/udp 7480

FROM base AS ibs
COPY --from=build /out/ibs /usr/local/bin/ibs
# 8080 admin UI, 9000 S3, 7443 site-to-site (TCP, and UDP for direct QUIC), 7480 storage node
EXPOSE 8080 9000 7443/tcp 7443/udp 7480
