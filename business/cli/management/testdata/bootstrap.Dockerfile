FROM alpine:3.22 AS trust
FROM scratch
COPY borq /borq
COPY --from=trust /etc/ssl/cert.pem /etc/ssl/certs/ca-certificates.crt
ENTRYPOINT ["/borq"]
