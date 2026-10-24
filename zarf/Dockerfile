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

FROM --platform=$TARGETPLATFORM debian:bookworm-slim

ARG TARGETOS
ARG TARGETARCH

WORKDIR /app

COPY --from=goose-builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=goose-builder /go/bin/goose /usr/local/bin/goose
COPY ${TARGETOS}/${TARGETARCH}/borq /usr/local/bin/borq

EXPOSE 7833

ENTRYPOINT ["/usr/local/bin/borq"]
CMD ["server", "--listen=0.0.0.0", "--data-dir=/var/lib/borq"]
