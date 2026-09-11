<p align="center">
  <img src="docs/images/victoria-gateway.jpg" alt="Victoria Gateway" width="820">
</p>

# Victoria Gateway

[![CI](https://github.com/GordonWei/victoria-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/GordonWei/victoria-gateway/actions/workflows/ci.yml)

> **TL;DR** — Alertmanager fires → Victoria Gateway pulls the matching Loki
> logs → a local LLM summarizes what's going on → the summary is pushed to
> Telegram. Optionally: escalate hard alerts to a cloud model, ground the
> analysis in similar past incidents (RAG), auto-file a Gitea/GitHub issue
> per alert. Single Go binary, no runtime dependency beyond Loki and an
> LLM endpoint.

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
Alertmanager --webhook--> victoria-gateway
                               |
                       [Basic Auth check]        optional (webhook_auth)
                               |
                       [dedup by fingerprint]     always on, 10 min window
                               |
                               v
                        query Loki for logs
                               |
                       [search past incidents]    optional (rag)
                               |            \
                               |             > added to the prompt as context
                               v            /
                      local LLM summarizes
                               |
                       [escalate]                 optional (cloud + escalation)
                               |
                 +------+------+------+
                 v      v             v
              Gemini  Anthropic   AWS DevOps Agent
              (re-analyze logs)   (MCP → investigate
                                   AWS account directly)
                               |
                 +-------------+-------------+
                 v                           v
          Telegram push              [capture incident +
                                       file tracker issue]
                                       optional (rag)
```

`POST /webhook/alertmanager` accepts Alertmanager's standard webhook payload
(one or more alerts). Each alert must carry either a `host`/`instance` label
or a `namespace`+`pod` pair — see **Identifying what an alert is about**,
below, for why there are two and how the right one is picked. Alerts with
neither are reported back as an error entry rather than failing the whole
request. `resolved` deliveries are never analyzed — they just clear the
dedup entry for that fingerprint so a genuinely new firing of the same
alert isn't mistaken for a duplicate.

### Identifying what an alert is about

Alerts arrive from two different worlds here, and they don't share a label
vocabulary. A `node_exporter`/blackbox-style alert (`InstanceDown`, disk,
CPU) carries `host` or `instance` — an IP:port or hostname — and its logs
live in Loki under the machine's `host` label (shipped by syslog/journald).
A Kubernetes alert sourced from `kube-state-metrics` carries `namespace`
instead — its `instance` label is kube-state-metrics' own scrape address,
not the affected workload's host — and its logs live in Loki under
`namespace`/`pod`/`container` (shipped by an in-cluster log agent such as
Grafana Alloy), a completely different set of labels.

The kube-state-metrics side further splits in two: a pod-level alert
(`KubePodCrashLooping`, `KubePodNotReady`) carries `namespace`+`pod` — the
most specific selector available. A workload-level alert (a Deployment or
StatefulSet replica-mismatch check) describes the Deployment/StatefulSet
object itself, which kube-state-metrics never attaches a `pod` label to —
there's no single pod the alert is "about" — so it falls back to a
namespace-only selector instead, which returns every pod's logs in that
namespace rather than a scoped-but-empty one.

Querying Loki with the wrong vocabulary doesn't error — `{host="..."}`
against a K8s alert is a perfectly well-formed query that always returns
zero rows, which silently starves both the LLM prompt and RAG capture of
log context while looking like everything worked. `Alert.AffectedIdentity()`
(`pkg/aiops/types.go`) is what closes that gap: pod-scoped when a `pod`
label is present, namespace-scoped when a `deployment`/`statefulset` label
confirms a workload-level alert, `host`/`instance` otherwise — and returns
both a human-readable display string (used for the LLM prompt, RAG's
stored record, and tracker issue titles — so a K8s alert reads
`gitea/gitea-585b7c9565-r2lc7` instead of a meaningless scrape address) and
the LogQL selector that actually finds the right logs. If you're adding a
third alert source with its own label shape, this is the function to teach
about it — the Loki client itself has no way to infer which selector shape
applies.

Everything in brackets above is optional and off unless configured:
authenticating the webhook (see **Securing the webhook**, below), grounding
the prompt in similar past incidents and capturing new ones (**RAG**,
below), and escalating a hard alert from the local model to a cloud model
(**Triage**, below). Fingerprint dedup is the one piece that's always on —
it costs nothing to leave enabled and protects against Alertmanager retries
double-processing the same alert.

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
  # api_key: ""      # optional; only for a real cloud endpoint requiring auth, see below
  # timeout_sec: 180 # optional; per-call timeout, default 60 — raise above your
                     # model server's cold-load time (LM Studio JIT reload after idle)

telegram:
  bot_token: ""   # leave empty to disable the push
  chat_id: 0
```

`loki.endpoint` and `summarizer.endpoint` are required — the process exits
on startup if either is missing. Everything else has a default or is
optional. See `deploy/config.docker.yaml` for the same shape with
container-specific comments (including the optional sections below).

`summarizer` is normally an unauthenticated local server, but
`summarizer.api_key` lets it be a real cloud OpenAI-compatible endpoint
instead — sent as `Authorization: Bearer <key>`. Useful if a local backend
is too slow or unreliable and you'd rather every alert go straight to cloud,
not just escalated ones (see Triage below). Gemini exposes an
OpenAI-compatible layer at
`https://generativelanguage.googleapis.com/v1beta/openai` that works here
with the same API key used for `cloud.api_key`.

### Securing the webhook

`/webhook/alertmanager` has no auth by default (matching Alertmanager's own
default-open `webhook_configs`), which is fine if it's only reachable from a
private network you already trust. If it's exposed more broadly, add
`webhook_auth` to require HTTP Basic Auth:

```yaml
webhook_auth:
  username: "alertmanager"
  password: "a-real-secret"
```

Requests without valid credentials get `401`, before any Loki/LLM/cloud/RAG
work happens. Configure the matching `basic_auth` on Alertmanager's side —
see `deploy/alertmanager_receiver_example.md`.

## Maintenance windows

For planned maintenance where you already know alerts will fire and don't
want to be paged (or don't want to waste LLM calls on expected noise), add
`maintenance_windows` to `config.yaml`:

```yaml
maintenance_windows:
  - name: "weekly-db-backup"
    schedule: "SAT 02:00-04:00"    # periodic: DOW HH:MM-HH:MM (see below)
    matchers:
      job: "postgres-backup"       # label name -> glob pattern
    action: "suppress"             # "suppress" or "mute"
    timezone: "Asia/Taipei"        # optional; see "Timezones" below

  - name: "one-off-migration"
    start: "2026-09-01T22:00:00Z"  # one-time: ISO8601, use this OR schedule
    end:   "2026-09-02T02:00:00Z"
    matchers:
      host: "db-primary"
    action: "mute"
```

Two actions, checked right after fingerprint dedup, before any Loki/LLM work
for a matching alert:

- **`suppress`** — skip entirely: no Loki query, no LLM call, no RAG
  capture, no Telegram push. Cheapest option; you get no record the alert
  fired at all.
- **`mute`** — still analyze and RAG-capture as usual, just skip the
  Telegram push. Use this when you still want the incident on record (e.g.
  synced to Gitea/GitHub for later reference) but don't want to be
  interrupted for something already expected.

`schedule` supports three periodic forms — `"SAT 02:00-04:00"` (every
Saturday), `"DAILY 04:00-04:30"` (every day), `"1st-SUN 03:00-06:00"` (only
the first Sunday of the month, or any `Nth-DOW` combination) — and a window
that crosses midnight (e.g. `"SAT 23:00-02:00"`) correctly stays scoped to
just the specified week for `Nth-DOW` schedules, not every occurrence of
that weekday. Exactly one of `schedule` or `start`+`end` must be set, never
both — `Validate()` rejects the config at startup otherwise. `matchers` must
be non-empty (refuses to accidentally match every alert) and each value
supports `*`/`?` wildcards as a normal string glob — including across `/`
(e.g. `"/api/*"` matches `/api/v1/checkout`), unlike Go's `filepath.Match`
which treats `/` as a path separator boundary.

### Timezones

By default, `schedule` times are evaluated in the process's local timezone
(`TZ` env var) — if the container's `TZ` isn't what you expect, a "every
Saturday 2am" window won't fire at 2am in the timezone you had in mind. Set
the optional per-window `timezone` field (an IANA zone name, e.g.
`"Asia/Taipei"`) to pin that window's schedule to a specific zone regardless
of what the process itself runs in — useful when the gateway runs in one
zone (say, a cloud region's UTC container) but the infrastructure the
window is really about "means" a different one. `timezone` only affects
`schedule`-based windows; a one-time `start`/`end` window is already an
absolute instant (RFC3339 carries its own offset) and ignores it. An
invalid zone name is rejected at config load / API validation time, same as
any other malformed window field.

### Hot-reloading windows: `GET`/`PUT /maintenance-windows`

Changing `maintenance_windows` normally means editing `config.yaml` and
restarting. `GET`/`PUT /maintenance-windows` let you inspect and replace the
whole window set at runtime — handy for a one-off "quiet this for the next
two hours" without a redeploy.

Gated by the same `webhook_auth` Basic Auth as `POST
/webhook/alertmanager`, when configured (unset means unauthenticated, same
as the webhook) — this endpoint can suppress or mute alert delivery, so
treat it as at least as sensitive.

```bash
# See what's currently in effect
curl -s http://localhost:8090/maintenance-windows | jq
# {"maintenance_windows":[{"name":"weekly-db-backup","schedule":"SAT 02:00-04:00", ...}]}

# Replace the whole set (not a merge — this is the complete new list)
curl -s -X PUT http://localhost:8090/maintenance-windows \
  -H "Content-Type: application/json" \
  -d '[
    {"name": "tonight-only", "start": "2026-09-06T22:00:00+08:00", "end": "2026-09-07T02:00:00+08:00",
     "matchers": {"host": "172.16.100.6"}, "action": "suppress"}
  ]'
# {"status":"ok","count":1}
```

The JSON body is an array of the same object shape as one
`maintenance_windows:` YAML entry (field names match: `name`, `schedule`,
`start`, `end`, `matchers`, `action`, `timezone`). `PUT` is all-or-nothing:
the whole array is validated and parsed before anything about the live
window set is touched, so an invalid submission (bad action, empty
matchers, unparseable schedule/timezone, ...) returns `400` with the
`config.Validate`-equivalent error message and leaves whatever was
previously in effect completely unchanged — never a partial replace.
Changes made this way don't touch `config.yaml` on disk; a process restart
reverts to whatever the file says, so a change you want to survive a
restart still needs to be written back to the file separately.

## Notification routing: multiple channels

By default there is one destination: the top-level `telegram` block, and
every alert's summary goes there. The optional `notifications` block turns
that into label-based routing across named channels:

```yaml
notifications:
  channels:
    - name: "critical-ops"
      type: telegram
      bot_token: "..."
      chat_id: -100123456789
    - name: "itsm-webhook"
      type: webhook
      url: "http://internal-itsm/api/v1/alerts"
      # headers: { Authorization: "Bearer ..." }   # optional
      # method: POST                                # optional
  routes:
    - matchers: { severity: critical, alertname: "Instance*" }
      channels: ["critical-ops", "itsm-webhook"]
    - default: true
      channels: ["critical-ops"]
```

Routes are a flat, first-match list — matchers use the exact same
label-glob syntax as maintenance windows (AND across matchers, `*`/`?`
globs that cross `/`). A `default: true` route matches everything and must
come last; a route with no default means unmatched alerts are deliberately
not delivered. One route can fan out to several channels; each delivery is
independent (one channel failing never blocks the others) and counted
per-channel at `/metrics` (`victoria_gateway_notify_push_total{channel=...}`
and `..._failure_total`).

Backward compatibility: with no `notifications` block, nothing changes.
With both, the top-level `telegram` becomes the implicit default route —
unless the routes already declare their own `default: true`, in which case
the top-level block is ignored (with a startup log line saying so).

`type: webhook` channels POST a JSON body mirroring the analysis
response's `alert_result` shape (`alert_name`, `host`, `summary`,
`analyzed_by`, plus `similar_incidents` when present).

Delivery reliability, both channel types: messages that fail on a
transient error (network error, 429, 5xx) are retried twice with short
backoff; permanent rejections (4xx) are not retried. Telegram messages are
truncated below the API's 4096-character limit (without splitting an HTML
entity or tag) instead of being silently rejected whole — the failure mode
before this existed was "long cloud analysis = no notification at all".

## Similar incidents in notifications, and the /incidents pages

When RAG is enabled, each notification can end with a "相似歷史事件"
section linking past *confirmed* incidents whose cosine similarity clears
`rag.similarity_threshold` (default 0.75; the summarizer prompt still gets
all `top_k` hits regardless — the threshold only gates what humans see).
Records that filed a tracker issue link to the issue; records without one
link to this service's own read-only pages:

- `GET /incidents` — recent confirmed incidents (`?limit=`, default 20, max
  100). Optional `?alertname=` and `?host=` narrow the list to records whose
  alert name / host contain that substring (case-insensitive, both filters
  ANDed together when both are given); the page also has a small filter
  form so this doesn't require hand-editing the URL, and the current
  filter values stay filled in after submitting. No filter means the same
  "just the recent list" behavior as before this existed.
- `GET /incidents/{id}` — one record: resolution, capture-time summary, log excerpt

Set `rag.public_base_url` to the address a human's browser can actually
reach so those links are absolute; disable the whole section with
`rag.show_similar_in_notification: false`.

## Async webhook mode and graceful shutdown

`webhook_async: true` makes `POST /webhook/alertmanager` answer
`202 Accepted` (with `{"status":"accepted","accepted":N}`) right after
filtering/dedup and run the analyses in background goroutines. Recommended
whenever a real Alertmanager is the caller: its webhook timeout is far
shorter than a minutes-long cloud escalation, so in the default
synchronous mode every slow analysis makes Alertmanager time out and
redeliver (absorbed by dedup, but noisy and pointless). The default stays
synchronous because the full-results response body is handy for curl-based
testing.

On SIGTERM/SIGINT the server stops accepting requests and waits up to
`shutdown_grace_sec` (default 300s) for in-flight analyses — including
async background ones — to finish before exiting, so a redeploy doesn't
kill a half-done escalation, RAG capture, or issue filing. Pair it with
`stop_grace_period` on the compose service (see
`deploy/docker-compose.snippet.yml`), or Docker's default 10s SIGKILL
lands first and the drain never happens.

## Triage: escalating a hard alert to a cloud model

The local model is fast and free, but it's a small quantized model — it can
misdiagnose alerts that need broader reasoning or turn up nothing when a
host's logs don't cover the real cause. Add a `cloud` block and victoria-gateway
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
  max_per_hour: 20   # optional, defaults to 0 (unlimited)
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

### Optional: escalating to AWS DevOps Agent instead of a chat completion

A third `provider` option, `"aws-devops-agent"`, escalates to
[AWS DevOps Agent](https://docs.aws.amazon.com/devopsagent/latest/userguide/)
over MCP instead of asking a bigger LLM the same question. The difference
matters: Gemini/Anthropic re-analyze whatever log excerpt Loki already gave
victoria-gateway, but AWS DevOps Agent goes and looks at the target AWS
account itself (CloudWatch metrics/alarms, CloudTrail, Lambda/EC2 config,
...) and comes back with an evidence-backed root cause — see
`pkg/model.DevOpsAgentClient` for the full design rationale.

```yaml
cloud:
  provider: "aws-devops-agent"
  aws_devops_agent:
    binary_path: "/path/to/venv/bin/aws-devops-agent"  # optional, defaults to $PATH
    user_id: "your-iam-username"
    region: "us-east-1"      # optional, defaults to us-east-1
    space_id: "..."          # the AgentSpace ID; see ONBOARDING.md below
    priority: "HIGH"         # optional, defaults to HIGH
```

This only makes sense when the alert being escalated is *about* AWS
infrastructure — AWS DevOps Agent has no visibility into on-prem/home-lab
hosts, only whatever AWS account and resources you've associated with the
AgentSpace.

The point of this provider isn't just "swap in a fancier model" — the full
alert context this client sends, including any RAG-retrieved history of
past, human-confirmed incidents (see the RAG section above), goes to AWS
DevOps Agent as the investigation's `description`, and whatever it
concludes flows back into the same RAG store afterward like any other
provider's result. Confirmed 2026-08-23 against the real service: the
description is delivered intact (verified by reading the created task back
via the AWS API). Whether a given investigation's *conclusion* ends up
citing that history is a separate question — in testing, the agent
consistently preferred re-deriving the answer from its own live AWS
evidence (CloudTrail, CloudWatch) over taking a supplied note at face
value, which is the same "verify, don't trust blindly" posture this
deployment already applies everywhere else, not a shortcoming.

It also pulls in a real dependency: the AgentSpace/MCP server
plumbing is
[aws-samples/sample-aws-devops-agent-acp-mcp](https://github.com/aws-samples/sample-aws-devops-agent-acp-mcp)
(`pip install -e '.[mcp]'`, Python 3.10+), which this client shells out to
as a subprocess and talks MCP to over stdio — a deliberate exception to
this repo's otherwise single-binary/minimal-dependency footprint, isolated
to deployments that opt into this one provider. Follow that repo's
`ONBOARDING.md` for AgentSpace creation and IAM setup
(`AIDevOpsAgentFullAccess` on your IAM user, plus an `AIDevOpsAgentAccessPolicy`
service role for the AgentSpace to assume) before enabling this.

Escalations here take 5-8 minutes (a real investigation, not a single
completion) rather than the few seconds Gemini/Anthropic take — factor that
into `escalation.max_per_hour` and expectations about how quickly an
escalated alert's result shows up.

`escalation.max_per_hour` is an optional spend guardrail: at most this many
alerts escalate to `cloud` within a rolling hour, 0 (default) meaning
unlimited. Nothing else bounds cost if the local model's `escalate` signal
misfires broadly or `always_cloud` matches more alerts than intended — an
alert that hits the cap stays on the local result (logged, not failed)
instead of also calling `cloud`.

## RAG: grounding the summary in past incidents

Optional, and off by default. When enabled, victoria-gateway embeds each new
alert and searches a Postgres+pgvector store for similar past incidents,
inserting whatever it finds into the prompt as reference context (both the
local and any escalated cloud call see it).

### The shape of this RAG, and its edges

If you've come in expecting document-RAG concerns — chunk size, overlap,
splitting strategy — this doesn't have any of that, on purpose. Each row in
`incidents` is one past alert: an alert name, a host, and a few hundred
characters of log/summary text. There's no long document to split, so
there's nothing to chunk.

What it does instead, deliberately:

- **Query and stored text are built the same shape.** `rag.BuildQueryText`
  is used for both what gets embedded when a record is captured and what
  gets embedded when a new alert is searched for — a query and the records
  it's meant to match need to look like the same *kind* of text for cosine
  similarity to mean anything. Field labels in that text are plain
  `key=value` ASCII tokens, not natural-language words, so the repo works
  the same whether `embedding_model` is a multilingual model or not.
- **Log context is the last N lines, not the first.** An incident's log
  excerpt is truncated to the most recent lines — the error is usually at
  the tail, not the head.
- **Only Confirmed records are retrieved.** Pending (unconfirmed, auto-
  captured) records are excluded from `Search` — an LLM's own guess isn't
  ground truth, and surfacing it as a "similar past incident" risks
  reinforcing a wrong guess.
- **Two thresholds, on purpose different.** `top_k` controls how many
  records the *summarizer prompt* sees (a weak match is still useful
  context for a model). `similarity_threshold` separately gates what a
  *human* sees in a notification (a weak match shown to a person reads as
  a claim, not a hint) — see [Similar incidents in
  notifications](#similar-incidents-in-notifications-and-the-incidents-pages).
- **A known, unresolved asymmetry.** The text embedded when a record is
  *captured* is built from the LLM's analysis summary (richer, written
  after investigation); the text embedded when a new alert is *queried*
  is built from Alertmanager's own (usually terse, templated)
  `description`/`summary` annotation. That's deliberate — the stored side
  is meant to be the more useful, information-dense half — but it rests on
  an unverified assumption that the two stay close enough in vector space
  for cosine similarity to still connect them. If your alerts' Alertmanager
  descriptions are very short relative to your summarizer's output, this is
  worth measuring for your own data before trusting retrieval quality.

What this means for scale: at home-lab volume (tens to low hundreds of
confirmed incidents), none of the usual document-RAG tuning knobs —
reranking, hybrid search, HNSW `ef_search`/`m` tuning — are worth adding.
`schema.sql` builds the HNSW index with pgvector's defaults, and at this
scale recall is not the bottleneck. If you're running this against a much
larger incident history (many thousands of confirmed rows), HNSW recall
under default parameters is where you'd start looking — but that's a
decision for whoever has that data and that failure mode in front of them,
not something this repo should preemptively build in.

**One failure mode is worth calling out specifically because it's silent.**
pgvector enforces the `embedding` column's *dimension*, not which model
produced a given vector. Swap `rag.embedding_model` to a different model
(same output dimension, or the same model name re-released with different
weights) and every write and read still succeeds — old rows just live in a
different coordinate space than new queries, so `Search` keeps returning
results, they're just increasingly meaningless, and nothing about that
errors. Every row records the `embedding_model` that actually produced its
vector, and victoria-gateway checks this at startup: if any row's recorded
model doesn't match what's currently configured (including rows from before
this column existed, which read as unknown), it logs a warning naming which
rows are affected. It's a warning, not a hard failure — you may already be
mid-migration — but it's the only thing standing between a silent model
swap and slowly-degrading answers nobody notices.

No Postgres already running? `deploy/rag-quickstart/` has a
`docker-compose.yml` that stands up pgvector and applies `schema.sql`
automatically — `cd deploy/rag-quickstart && docker compose up -d` gets you
a `postgres_dsn` to paste below in about a minute, no manual pgvector
package install needed.

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

**Only Confirmed records are ever retrieved.** Every analyzed alert gets
captured as a Pending record automatically (no config needed beyond `rag`
itself) — but Pending records don't show up in Search, because an LLM's
own guess about an alert isn't confirmed truth, and retrieving unconfirmed
guesses risks a wrong one getting repeated later. A record only becomes
Confirmed, and therefore retrievable, once a human attaches a real
resolution. Two ways to do that:

**Manually**, any time, for any record (whether or not it came from an
auto-capture):

```bash
victoria-gateway note --id 42 --resolution "舊測試機殘留的 scrape target，機器已下線，從 node.yml 拿掉了"

# or, for an incident that predates capture / was never auto-captured:
victoria-gateway note \
  --alert-name "InstanceDown" --host "172.16.100.7" \
  --resolution "舊測試機殘留的 scrape target，機器已下線，從 node.yml 拿掉了"
```

`--resolution` is always required, and either `--id` (confirms an existing
Pending record) or `--alert-name` (creates a new Confirmed one from
scratch) must be given.

**Automatically, via Gitea or GitHub**, if you add a `gitea` or `github`
block (configure at most one — having both set is a startup error):

```yaml
rag:
  enabled: true
  postgres_dsn: "postgres://user:pass@host:5432/dbname"
  embedding_endpoint: "http://your-llm-host:1234"
  embedding_model: "bge-m3"
  gitea:
    endpoint: "https://your-gitea-instance"
    token: "..."          # needs write:issue scope on the target repo
    owner: "your-username"
    repo: "victoria-gateway-incidents"   # a dedicated repo, not the code repo
```

or, on GitHub instead:

```yaml
rag:
  enabled: true
  postgres_dsn: "postgres://user:pass@host:5432/dbname"
  embedding_endpoint: "http://your-llm-host:1234"
  embedding_model: "bge-m3"
  github:
    # endpoint: ""   # optional, defaults to https://api.github.com; set for GitHub Enterprise Server
    token: "..."          # needs the "issues" repo permission on the target repo
    owner: "your-username"
    repo: "victoria-gateway-incidents"   # a dedicated repo, not the code repo
```

With either set, every analyzed alert also files an issue (title = alert
name + host, body = the analysis) alongside the Pending record it's linked
to. Investigate as normal; before closing the issue, leave one comment
describing what it actually was — that's what gets pulled back as the
resolution. Then run:

```bash
victoria-gateway sync
```

`sync` checks every Pending record with a linked issue, and for any issue
that's since closed, reads its last comment in as the resolution and marks
the record Confirmed. It's meant to run on a schedule (cron), not stay
running — one pass, then exit. An issue closed with no comment is left
Pending; there's nothing to confirm it with, and `note --id` still works on
it by hand later.

Retrieval and capture failures (embedding endpoint down, Postgres or the
issue tracker unreachable) are logged and treated as "skip this part" rather
than failing the alert — none of RAG is a dependency the core summarizer
needs to stay up.

Setup, once, before turning `rag.enabled` on:

1. Get a Postgres with the pgvector extension available. Easiest path:
   `cd deploy/rag-quickstart && docker compose up -d` (uses the
   `pgvector/pgvector` image, which ships the extension binary preinstalled,
   and applies `schema.sql` automatically on first start). Otherwise, install
   the pgvector extension on your existing Postgres server yourself (an
   OS/apt package — `CREATE EXTENSION` alone won't work if the extension
   binary isn't installed) and run `pkg/rag/schema.sql` against the target
   database by hand (already-deployed databases from before the
   Pending/Confirmed split should run `pkg/rag/migrate_0001_pending_status.sql`
   instead, and databases from before the `embedding_model` column existed
   should also run `pkg/rag/migrate_0002_embedding_model.sql` — both upgrade
   an existing `incidents` table in place, a fresh install needs neither).
   Both schema files default the embedding column to `vector(1024)`, matching
   `bge-m3`'s output dimension — if you use a different embedding model,
   check its dimension and edit the column definition before running either
   one.
2. Point `embedding_endpoint`/`embedding_model` at wherever that model is
   served (the same LM Studio/Ollama/vLLM instance `summarizer` uses is
   fine, as long as it also has an embedding model loaded).
3. If using Gitea or GitHub capture, create a dedicated repo for issues
   first (don't reuse the code repo) and generate a token scoped to
   `write:issue` (Gitea; plus `write:repository`/`write:user` if creating
   the repo via the API too, as opposed to the web UI) or the `issues` repo
   permission (GitHub, fine-grained PAT).

### Getting a Telegram bot token and chat ID

1. Message [@BotFather](https://t.me/BotFather) on Telegram, send `/newbot`,
   follow the prompts. You get a token back that looks like
   `123456789:AAExampleTokenNotReal`.
2. Send any message to your new bot (DM it directly, or add it to a group).
3. Hit `https://api.telegram.org/bot<token>/getUpdates` in a browser or with
   curl. The `chat.id` field in the response is your `chat_id`.

## Running

```bash
go build -o victoria-gateway ./cmd/victoria-gateway/
VICTORIA_GATEWAY_CONFIG=./config.yaml ./victoria-gateway
```

Config path resolution: `--config <path>` flag, else `$VICTORIA_GATEWAY_CONFIG`,
else `/etc/victoria-gateway/config.yaml`. `--port <n>` overrides `listen_addr`
from the config file if you need to override it without editing the file.

```bash
./victoria-gateway --config ./config.yaml --port 9000
```

`GET /healthz` returns `200 ok` once the process is up — use it for a
container healthcheck or a quick "is this running" check.

`GET /metrics` exposes counters in Prometheus text exposition format —
alerts processed/errored, dedup/resolved skips, webhook auth rejections,
escalation attempts/failures/rate-limits, RAG capture/search failures,
tracker issue-creation failures, per-channel notification pushes/failures
(`victoria_gateway_notify_push_total{channel=...}`) — plus duration
sum/count pairs (`victoria_gateway_analysis_duration_seconds_sum`/`_count`,
and the same for `loki_query`, `local_llm`, `cloud_llm`, `rag_search`)
so a dashboard can graph average latencies ("is the local model slower
today"). No external dependency for this (`pkg/metrics` is hand-rolled,
not `prometheus/client_golang`) — point a Prometheus scrape config at this
port's `/metrics` path if you want them collected.

Or via Docker — see `Dockerfile` and `deploy/`:

```bash
docker build -t victoria-gateway:latest .
```

`deploy/docker-compose.snippet.yml` has the service block to add to an
existing docker-compose stack (same network as Loki/Prometheus/Alertmanager).
`deploy/alertmanager_receiver_example.md` covers wiring it into an existing
Alertmanager route as an additive second webhook target.

## Status

Running in production against a home Alertmanager/Loki/LM Studio stack,
with CI (gofmt/build/vet/`go test -race`/golangci-lint) on every push and
PR. Every feature above — Triage, RAG capture/retrieval, the Gitea/GitHub
tracker integrations, webhook auth, fingerprint dedup, concurrent
multi-alert processing, maintenance windows — has been exercised against
real alerts on that deployment, not just unit tests.

Maintenance windows specifically: shipped 2026-08-27, with two real bugs
found in review before it reached production — a cross-midnight schedule
combined with `Nth-DOW` matched every occurrence of that weekday instead of
just the specified one, and the glob matcher (`filepath.Match`) silently
failed to match any pattern containing `/`, which is common in label values
like paths — both fixed with regression tests, then re-verified end-to-end
against the live deployment (a synthetic alert matching a temporary test
window was confirmed suppressed; a non-matching one correctly proceeded
through the full pipeline). See `pkg/maintenance/maintenance_test.go` for
the regression cases.

2026-08-31 batch — v1.2.0 (notification routing, similar-incident
references, the `/incidents` pages, async webhook mode, graceful
shutdown, Telegram truncation/retry, Loki/embedder retry, duration
metrics): implemented with unit coverage across `pkg/notify`,
`pkg/config`, `pkg/rag`, and `cmd/victoria-gateway`, then deployed to the
production monitoring host the same day and verified end-to-end with a
real test alert: 202 in ~5ms (async mode), Loki query, RAG retrieval,
local summarize, a genuine local-model-requested cloud escalation,
Telegram delivery, RAG capture and tracker issue filing all confirmed via
logs and the new duration/notify metrics (test artifacts cleaned up
afterwards). One pre-existing behavior observed, not caused by this
batch: the local LLM's 60s default client timeout can fail the first
analysis after the model server has been idle (cold model load); the
retry lands on a warm model. Fixed properly in v1.2.1 via
`summarizer.timeout_sec` — set it above the cold-load time (e.g. 180).

Started as a subcommand of a larger CLI project; this repo is that logic
extracted and trimmed down to just what the service needs (no CLI, REPL, or
unrelated routing/session code came along).

## Testing

```bash
go test ./...
```

## Artwork

The header image (`docs/images/victoria-gateway.jpg`) and the repository's
social preview (`docs/images/social-preview.jpg`) were generated with ChatGPT.
Both are the same artwork; the preview is only re-framed to 1280x640 so that
link unfurls do not crop it.

## License

MIT — see [LICENSE](LICENSE).
