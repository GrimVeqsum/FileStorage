# Upstream distributes the community edition as source only. Build its pinned
# security release rather than relying on a removed registry image or :latest.
# Source/license: https://github.com/minio/minio/tree/RELEASE.2025-10-15T17-29-55Z
FROM golang:1.26.10-alpine3.24 AS builder
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOBIN=/out
RUN go install -trimpath github.com/minio/minio@RELEASE.2025-10-15T17-29-55Z
RUN cp /go/pkg/mod/github.com/minio/minio@*/LICENSE /out/LICENSE

FROM alpine:3.24.3
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/minio /usr/local/bin/minio
COPY --from=builder /out/LICENSE /usr/share/licenses/minio/LICENSE
EXPOSE 9000 9001
# Match the original image's writable volume behavior for existing user data.
ENTRYPOINT ["/usr/local/bin/minio"]
CMD ["server", "/data", "--console-address", ":9001"]
