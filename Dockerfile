FROM --platform=linux/amd64 golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /server ./cmd/app

FROM --platform=linux/amd64 alpine:3.20

WORKDIR /app

COPY --from=builder /server /app/server

RUN chmod +x /app/server

EXPOSE 8080

CMD ["/app/server"]