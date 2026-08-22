# victoria-gateway container image: receives Alertmanager webhooks, pulls the
# surrounding Loki logs for the alerting host, asks a local LLM to
# summarize what's going on, and pushes the summary to Telegram. See
# deploy/ for docker-compose wiring.

FROM golang:1.25 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-X main.version=docker" -o /out/victoria-gateway ./cmd/victoria-gateway/

# Alpine, not distroless: when this misbehaves in a home lab at 11pm, a
# `docker exec sh` to poke at the mounted config beats reaching for a debug
# sidecar. The image is still small (Alpine base + one static binary).
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
RUN adduser -D -H -u 10001 victoriagw
COPY --from=builder /out/victoria-gateway /usr/local/bin/victoria-gateway
USER victoriagw

ENV VICTORIA_GATEWAY_CONFIG=/etc/victoria-gateway/config.yaml
EXPOSE 8090

ENTRYPOINT ["victoria-gateway"]
