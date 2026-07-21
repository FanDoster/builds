# Build stage
FROM golang:alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /builds ./cmd/builds

# Runtime
FROM alpine:3.20
RUN apk add --no-cache git docker-cli docker-cli-compose ca-certificates curl

COPY --from=builder /builds /usr/local/bin/builds

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/builds"]
