FROM golang:1.26.10-alpine3.24 AS builder
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN go build -trimpath -ldflags='-s -w' -o /out/server ./cmd/app

FROM alpine:3.24.3
RUN addgroup -S -g 10001 app && adduser -S -D -H -u 10001 -G app app
WORKDIR /app
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder --chown=app:app /out/server /app/server
USER app:app
EXPOSE 8080
ENTRYPOINT ["/app/server"]
