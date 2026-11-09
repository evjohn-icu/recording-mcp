# ---- Go build -------------------------------------------------------------
FROM golang:1.27.1-bookworm AS go-build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY fixtures ./fixtures
COPY Makefile ./
RUN make build

# ---- Frontend build -------------------------------------------------------
FROM node:22-bookworm-slim AS web-build
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ---- Runtime --------------------------------------------------------------
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl ffmpeg tini tzdata \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -u 10001 -r -s /usr/sbin/nologin app \
 && mkdir -p /app /data/profiles /data/audio \
 && chown -R app:app /data
COPY --from=go-build /src/bin/ /app/
COPY --from=web-build /src/web/dist /app/web/dist
COPY scripts/docker-profile-init.sh /app/docker-profile-init.sh
RUN chmod 0555 /app/docker-profile-init.sh
ENV HOME=/tmp
USER app
WORKDIR /app
ENTRYPOINT ["/usr/bin/tini","--"]
CMD ["/app/context-web","--help"]
