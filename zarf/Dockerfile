ARG BUILDPLATFORM
ARG TARGETPLATFORM
ARG TARGETOS=linux
ARG TARGETARCH=amd64

FROM --platform=$BUILDPLATFORM golang:1.25 AS goose-builder

ARG TARGETOS
ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go install github.com/pressly/goose/v3/cmd/goose@v3.26.0 && \
    GOOSE_BIN="/go/bin/${TARGETOS}_${TARGETARCH}/goose"; \
    if [ -f "$GOOSE_BIN" ]; then cp "$GOOSE_BIN" /go/bin/goose; fi

FROM --platform=$TARGETPLATFORM debian:bookworm-slim

ARG TARGETOS
ARG TARGETARCH

WORKDIR /app

RUN apt-get update && \
    apt-get install -y ca-certificates --no-install-recommends && \
    rm -rf /var/lib/apt/lists/*

COPY --from=goose-builder /go/bin/goose /usr/local/bin/goose
COPY ${TARGETOS}/${TARGETARCH}/borq /usr/local/bin/borq

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/borq"]
CMD ["management", "server"]
