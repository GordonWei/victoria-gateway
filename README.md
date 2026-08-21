# orch-gateway

An Alertmanager webhook receiver: on each incoming alert, it pulls the
surrounding Loki logs for the alerting host, asks a local OpenAI-compatible
LLM (LM Studio, Ollama, vLLM, etc.) to summarize what's going on, and pushes
that summary to Telegram.

Alertmanager only checks a webhook's HTTP status code — it never reads the
response body. So the Telegram push is the only place the LLM's answer
actually reaches anyone; without it, the summary would be computed and then
silently discarded.

## How it works

```
Alertmanager --webhook--> orch-gateway --query--> Loki
                               |
                               v
                       LLM (summarize)
                               |
                               v
                          Telegram push
```

`POST /webhook/alertmanager` accepts Alertmanager's standard webhook payload
(one or more alerts). Each alert must carry a `host` or `instance` label —
that's what scopes the Loki query. Alerts without one are reported back as
an error entry rather than failing the whole request.

## Config

See `deploy/config.docker.yaml` for the full shape. Four things to fill in:
Loki endpoint, LLM endpoint + model, Telegram bot token + chat ID. Leave
`telegram.bot_token` empty to disable the push (the summary still gets
computed, it just won't go anywhere).

## Running

```bash
go build -o orch-gateway ./cmd/orch-gateway/
ORCH_GATEWAY_CONFIG=./config.yaml ./orch-gateway
```

Or via Docker — see `Dockerfile` and `deploy/`:

```bash
docker build -t orch-gateway:latest .
```

`deploy/docker-compose.snippet.yml` has the service block to add to an
existing docker-compose stack (same network as Loki/Prometheus/Alertmanager).
`deploy/alertmanager_receiver_example.md` covers wiring it into an existing
Alertmanager route as an additive second webhook target.

## Status

This repo is a fresh extraction of a service that's been running in
production against a home Alertmanager/Loki/LM Studio stack — the code here
is the same logic, trimmed down to just what the service needs (no CLI,
REPL, or unrelated routing/session code from the project it started as a
subcommand of).

**Not yet done: the live deployment hasn't been cut over to build from this
repo** — it's still running off a build from its old location. Cutting over
means getting this repo (or its built image) onto the deployment host and
swapping the compose service's build source, which is a separate step.

## Testing

```bash
go test ./...
```
