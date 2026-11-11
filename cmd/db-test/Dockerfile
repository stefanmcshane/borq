FROM debian:bookworm-slim
COPY db-test /usr/local/bin/db-test
COPY --from=golang:1.25-alpine /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN mkdir -p /var/lib/borq/sqlite && chown 10001:2000 /var/lib/borq/sqlite && chmod 2775 /var/lib/borq/sqlite
ENV SQLITE_DATABASE_PATH=/var/lib/borq/sqlite/app.sqlite
USER 10001:2000
ENTRYPOINT ["/usr/local/bin/db-test"]
CMD ["serve"]
