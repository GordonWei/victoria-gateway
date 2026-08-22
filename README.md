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

## Requirements

- Go 1.25+ (see `go.mod`) to build from source
- Docker, only if you're using the container path instead

## Config

`config.yaml`, flat structure:

```yaml
listen_addr: ":8090"   # optional, defaults to :8090

loki:
  endpoint: "http://loki:3100"
  lookback_sec: 300     # optional, defaults to 300
  limit: 200             # optional, defaults to 200

summarizer:
  endpoint: "http://your-llm-host:1234"   # OpenAI-compatible /v1/chat/completions
  model: "your-model-name"

telegram:
  bot_token: ""   # leave empty to disable the push
  chat_id: 0
```

`loki.endpoint` and `summarizer.endpoint` are required — the process exits
on startup if either is missing. Everything else has a default or is
optional. See `deploy/config.docker.yaml` for the same shape with
container-specific comments.

### Getting a Telegram bot token and chat ID

1. Message [@BotFather](https://t.me/BotFather) on Telegram, send `/newbot`,
   follow the prompts. You get a token back that looks like
   `123456789:AAExampleTokenNotReal`.
2. Send any message to your new bot (DM it directly, or add it to a group).
3. Hit `https://api.telegram.org/bot<token>/getUpdates` in a browser or with
   curl. The `chat.id` field in the response is your `chat_id`.

## Running

```bash
go build -o orch-gateway ./cmd/orch-gateway/
ORCH_GATEWAY_CONFIG=./config.yaml ./orch-gateway
```

Config path resolution: `--config <path>` flag, else `$ORCH_GATEWAY_CONFIG`,
else `/etc/orch-gateway/config.yaml`. `--port <n>` overrides `listen_addr`
from the config file if you need to override it without editing the file.

```bash
./orch-gateway --config ./config.yaml --port 9000
```

`GET /healthz` returns `200 ok` once the process is up — use it for a
container healthcheck or a quick "is this running" check.

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

The live deployment has been cut over to build from this repo (2026-08-22):
the deployment host clones this repo directly and builds the image from it,
rather than a source transfer from the old location. The config schema is
flat (see Config above) — the old deployment's nested `aiops:`/`memory:`
schema from before the extraction is no longer supported.

## Testing

```bash
go test ./...
```

## License

MIT — see [LICENSE](LICENSE).
