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

Two things above are optional and off unless configured: escalating a hard
alert from the local model to a cloud model (**Triage**, below), and
grounding the prompt in similar past incidents (**RAG**, below).

## Requirements

- Go 1.25+ (see `go.mod`) to build from source
- Docker, only if you're using the container path instead
- Postgres with the pgvector extension, only if you enable RAG (see below)

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
container-specific comments (including the two optional sections below).

## Triage: escalating a hard alert to a cloud model

The local model is fast and free, but it's a small quantized model — it can
misdiagnose alerts that need broader reasoning or turn up nothing when a
host's logs don't cover the real cause. Add a `cloud` block and orch-gateway
can re-run the analysis against a stronger cloud model for alerts that need
it, while everything else still stays local. Gemini is the default provider
(`pkg/model.GeminiClient`); Anthropic is also supported
(`pkg/model.AnthropicClient`) via `provider: "anthropic"`:

```yaml
cloud:
  provider: "gemini"   # optional, defaults to "gemini"; "anthropic" also supported
  endpoint: ""   # optional; each provider has its own default
  api_key: "AIza..."
  model: "gemini-2.5-flash"

escalation:
  always_cloud:
    - "SomeAlwaysComplexAlert"   # alertname values, case-insensitive
```

Two independent signals decide whether an alert escalates, OR'd together:

1. **Your own rules** (`escalation.always_cloud`) — alert names you already
   know are complex or sensitive in your environment always escalate,
   regardless of what the local model thinks.
2. **The local model's own judgment** — the summarizer prompt asks the local
   model to reply with structured JSON (`summary`/`confidence`/`escalate`/
   `reason`), and `escalate: true` in that reply also triggers a re-run.

Rule (1) exists because small local models aren't reliably calibrated about
their own confidence — an explicit allowlist you control is deterministic in
a way a model's self-report isn't. Escalating doesn't send both answers to
Telegram; the cloud result replaces the local one (marked 🔍 instead of 🚨 in
the push, and `analyzed_by: "cloud"` in the JSON response) so you get one
answer, not a diff to reconcile yourself. If the cloud call itself fails
(bad key, network issue), the local result is used as a fallback rather than
failing the alert outright — check the process logs for
`cloud escalation failed` if that happens.

Leaving `cloud` unset (the default) disables all of this — `escalation` with
no `cloud` configured is a startup error rather than a silent no-op.

## RAG: grounding the summary in past incidents

Optional, and off by default. When enabled, orch-gateway embeds each new
alert and searches a Postgres+pgvector store for similar past incidents,
inserting whatever it finds into the prompt as reference context (both the
local and any escalated cloud call see it).

```yaml
rag:
  enabled: true
  postgres_dsn: "postgres://user:pass@host:5432/dbname"
  embedding_endpoint: "http://your-llm-host:1234"   # OpenAI-compatible /v1/embeddings
  embedding_model: "bge-m3"
  top_k: 3   # optional, defaults to 3
```

Setup, once, before turning this on:

1. Install the pgvector extension on the Postgres server itself (an OS/apt
   package — `CREATE EXTENSION` alone won't work if the extension binary
   isn't installed).
2. Run `pkg/rag/schema.sql` against the target database. It defaults the
   embedding column to `vector(1024)`, matching `bge-m3`'s output dimension
   — if you use a different embedding model, check its dimension and edit
   the column definition before running the migration.
3. Point `embedding_endpoint`/`embedding_model` at wherever that model is
   served (the same LM Studio/Ollama/vLLM instance `summarizer` uses is
   fine, as long as it also has an embedding model loaded).

**The store starts empty on purpose.** orch-gateway never writes to it
automatically — an LLM's own guess about an alert isn't confirmed truth,
and seeding the store with unconfirmed guesses risks a wrong guess getting
retrieved and repeated later. The only way a record gets in is the `note`
subcommand, run by a human after they've actually confirmed what an alert
turned out to be:

```bash
orch-gateway note \
  --alert-name "InstanceDown" \
  --host "192.0.2.7" \
  --resolution "舊測試機殘留的 scrape target，機器已下線，從 node.yml 拿掉了" \
  --log-excerpt "optional: paste whatever log line mattered" \
  --summary "optional: what the AI said at the time, for your own reference"
```

`--alert-name` and `--resolution` are required; everything else is optional
context. This reads the same `config.yaml` as the server (`--config` flag or
`$ORCH_GATEWAY_CONFIG`) and needs `rag.enabled: true` with all three RAG
fields set — it's meant to be run on whatever host can reach the Postgres
and embedding endpoint, not necessarily the deployment host itself.

Retrieval failures (embedding endpoint down, Postgres unreachable) are
logged and treated as "no context found" rather than failing the alert —
RAG is a prompt enhancement, not a dependency the core summarizer needs to
stay up.

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
