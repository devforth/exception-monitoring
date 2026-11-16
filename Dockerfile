# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/exception-monitoring ./cmd/exception-monitoring

# Data dir owned by the unprivileged runtime user (named volumes inherit this ownership).
RUN mkdir -p /out/data && chown 65534:65534 /out/data


FROM scratch

# CA certificates for outgoing TLS (SMTP, Telegram, Slack).
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/exception-monitoring /exception-monitoring
COPY --from=build --chown=65534:65534 /out/data /data

USER 65534:65534
ENV PORT=8000 \
    DATA_DIR=/data
VOLUME /data
EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/exception-monitoring", "healthcheck"]

ENTRYPOINT ["/exception-monitoring"]
