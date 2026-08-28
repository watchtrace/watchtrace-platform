FROM --platform=$BUILDPLATFORM golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY cmd ./cmd
COPY db ./db
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags="-s -w" \
    -o /out/watchtrace-api ./cmd/api && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags="-s -w" \
    -o /out/watchtrace-migrate ./cmd/migrate && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags="-s -w" \
    -o /out/watchtrace-monitor-engine ./cmd/monitor-engine && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags="-s -w" \
    -o /out/watchtrace-notification-worker ./cmd/notification-worker && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags="-s -w" \
    -o /out/watchtrace-worker-pool ./cmd/worker-pool && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags="-s -w" \
    -o /out/watchtrace-queue-admin ./cmd/queue-admin && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags="-s -w" \
    -o /out/watchtrace-deployment-keys ./cmd/deployment-keys && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags="-s -w" \
    -o /out/watchtrace-healthcheck ./cmd/healthcheck

FROM scratch

LABEL org.opencontainers.image.title="WatchTrace Platform" \
      org.opencontainers.image.description="WatchTrace backend control-plane commands" \
      org.opencontainers.image.source="https://github.com/watchtrace/watchtrace-platform"

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=65532:65532 /out/watchtrace-api /watchtrace-api
COPY --from=build --chown=65532:65532 /out/watchtrace-migrate /watchtrace-migrate
COPY --from=build --chown=65532:65532 /out/watchtrace-monitor-engine /watchtrace-monitor-engine
COPY --from=build --chown=65532:65532 /out/watchtrace-notification-worker /watchtrace-notification-worker
COPY --from=build --chown=65532:65532 /out/watchtrace-worker-pool /watchtrace-worker-pool
COPY --from=build --chown=65532:65532 /out/watchtrace-queue-admin /watchtrace-queue-admin
COPY --from=build --chown=65532:65532 /out/watchtrace-deployment-keys /watchtrace-deployment-keys
COPY --from=build --chown=65532:65532 /out/watchtrace-healthcheck /watchtrace-healthcheck

ENV WATCHTRACE_HTTP_ADDRESS=0.0.0.0:8080

USER 65532:65532
EXPOSE 8080
STOPSIGNAL SIGTERM

CMD ["/watchtrace-api"]
