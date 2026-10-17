FROM golang:1.27.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY web/index.html ./web/index.html
COPY web/src/ ./web/src/
COPY web/vendor/ ./web/vendor/
COPY docs/pure-go-runtime/THIRD_PARTY_NOTICES.txt /out/AUTH_RUNTIME_THIRD_PARTY_NOTICES.txt
RUN CGO_ENABLED=0 go run ./cmd/build-web
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/tosub2 ./cmd/tosub2

FROM debian:bookworm-slim AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 tosub2 \
    && useradd --uid 10001 --gid 10001 --create-home --home-dir /home/tosub2 --shell /usr/sbin/nologin tosub2 \
    && mkdir -p /data \
    && chown tosub2:tosub2 /data \
    && chmod 700 /data /home/tosub2
COPY --from=build /out/tosub2 /usr/local/bin/tosub2
COPY --from=build /out/AUTH_RUNTIME_THIRD_PARTY_NOTICES.txt /usr/share/doc/tosub2/AUTH_RUNTIME_THIRD_PARTY_NOTICES.txt
ENV ONBOARDING_HOST=0.0.0.0 \
    ONBOARDING_PORT=4399 \
    ONBOARDING_OUTPUT_ROOT=/data
USER 10001:10001
WORKDIR /data
EXPOSE 4399
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 \
    CMD curl --fail --silent --max-time 2 http://127.0.0.1:4399/api/health > /dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/tosub2"]
