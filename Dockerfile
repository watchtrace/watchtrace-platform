FROM --platform=$BUILDPLATFORM golang:1.26.5-bookworm@sha256:6c5605ab3a9a9fb3c4eafe5b3d63cdbf3881caf113262b67862547b54a9db599 AS build

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
    -o /out/watchtrace-migrate ./cmd/migrate

FROM scratch

LABEL org.opencontainers.image.title="WatchTrace Platform" \
      org.opencontainers.image.description="WatchTrace backend API and migration command" \
      org.opencontainers.image.source="https://github.com/watchtrace/watchtrace-platform"

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=65532:65532 /out/watchtrace-api /watchtrace-api
COPY --from=build --chown=65532:65532 /out/watchtrace-migrate /watchtrace-migrate

ENV WATCHTRACE_HTTP_ADDRESS=0.0.0.0:8080

USER 65532:65532
EXPOSE 8080
STOPSIGNAL SIGTERM

CMD ["/watchtrace-api"]
