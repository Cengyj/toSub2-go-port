FROM golang:1.26.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/tosub2 ./cmd/tosub2

FROM debian:bookworm-slim AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates chromium fonts-noto-core fonts-noto-cjk curl procps \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 tosub2 \
    && useradd --uid 10001 --gid 10001 --create-home --home-dir /home/tosub2 --shell /usr/sbin/nologin tosub2 \
    && mkdir -p /data \
    && chown tosub2:tosub2 /data \
    && chmod 700 /data /home/tosub2
COPY --from=build /out/tosub2 /usr/local/bin/tosub2
ENV CHROME_PATH=/usr/bin/chromium \
    ONBOARDING_HOST=0.0.0.0 \
    ONBOARDING_PORT=4399 \
    ONBOARDING_OUTPUT_ROOT=/data \
    XDG_CONFIG_HOME=/tmp/tosub2-config \
    XDG_CACHE_HOME=/tmp/tosub2-cache
USER 10001:10001
WORKDIR /data
EXPOSE 4399
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 \
    CMD curl --fail --silent --max-time 2 http://127.0.0.1:4399/api/health > /dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/tosub2"]
