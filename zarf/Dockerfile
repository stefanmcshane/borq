ARG BUILDPLATFORM
ARG TARGETPLATFORM

FROM --platform=$BUILDPLATFORM golang:1.25 AS goose-builder

ARG TARGETOS
ARG TARGETARCH

RUN --mount=type=secret,id=proxy_ca \
    if [ -f /run/secrets/proxy_ca ]; then export SSL_CERT_FILE=/run/secrets/proxy_ca; fi; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go install github.com/pressly/goose/v3/cmd/goose@v3.26.0 && \
    GOOSE_BIN="/go/bin/${TARGETOS}_${TARGETARCH}/goose"; \
    if [ -f "$GOOSE_BIN" ]; then cp "$GOOSE_BIN" /go/bin/goose; fi

FROM --platform=$TARGETPLATFORM restic/restic:0.18.0@sha256:4cf4a61ef9786f4de53e9de8c8f5c040f33830eb0a10bf3d614410ee2fcb6120 AS restic

FROM --platform=$TARGETPLATFORM debian:bookworm-slim

ARG TARGETOS
ARG TARGETARCH

WORKDIR /app

COPY --from=goose-builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=goose-builder /go/bin/goose /usr/local/bin/goose
COPY --from=restic /usr/bin/restic /usr/local/bin/restic
COPY ${TARGETOS}/${TARGETARCH}/borq /usr/local/bin/borq

EXPOSE 7833

ENTRYPOINT ["/usr/local/bin/borq"]
CMD ["server", "--listen=0.0.0.0", "--data-dir=/var/lib/borq"]
