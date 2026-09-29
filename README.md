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

Swimlanes below group each step by who actually does it — useful for seeing
at a glance what's Victoria Gateway's own logic versus an external system
it's calling out to.

```mermaid
flowchart LR
    AM(["Alertmanager<br/>fires alert"]) -->|webhook| B1

    subgraph VG["Victoria Gateway"]
        direction LR
        B1["Basic Auth check<br/><i>optional</i>"] --> B2["dedup by fingerprint<br/><i>always on</i>"] --> B3["query Loki<br/>for logs"] --> B4["search past<br/>incidents<br/><i>optional — rag</i>"]
    end

    subgraph LLM["Local LLM"]
        C1["summarize"]
    end

    B4 --> C1
    C1 --> JUDGE

    subgraph JUDGE["Escalation judge<br/><i>optional — additive only</i>"]
        J1["Jev (TypeSafe AI)<br/>independent severity/<br/>escalate read"]
    end

    JUDGE --> CLOUD

    subgraph CLOUD["Escalate to<br/>cloud <i>(optional)</i>"]
        direction TB
        D1["Gemini"] ~~~ D2["Anthropic"] ~~~ D3["Bedrock"] ~~~ D4["Azure OpenAI"] ~~~ D5["AWS DevOps Agent<br/><i>MCP, investigates the<br/>AWS account directly</i>"]
    end

    CLOUD --> OUT

    subgraph OUT["Output"]
        direction TB
        E1["Telegram push"] ~~~ E2["capture incident +<br/>file tracker issue<br/><i>optional — rag</i>"]
    end

    classDef trigger fill:#8ecae6,stroke:#0b5f7a,stroke-width:1.5px,color:#03045e;
    classDef gateway fill:#95d5b2,stroke:#2d6a4f,stroke-width:1.5px,color:#1b4332;
    classDef llm fill:#cdb4db,stroke:#5a189a,stroke-width:1.5px,color:#3c096c;
    classDef judge fill:#9bf6ff,stroke:#0077b6,stroke-width:1.5px,color:#03045e;
    classDef cloud fill:#ffd166,stroke:#b07d1a,stroke-width:1.5px,color:#5c3d05;
    classDef output fill:#ffafcc,stroke:#c9184a,stroke-width:1.5px,color:#590d22;

    class AM trigger;
    class B1,B2,B3,B4 gateway;
    class C1 llm;
    class J1 judge;
    class D1,D2,D3,D4,D5 cloud;
    class E1,E2 output;

    style VG fill:#eafaf1,stroke:#2d6a4f,stroke-width:1.5px;
    style LLM fill:#f3ecf9,stroke:#5a189a,stroke-width:1.5px;
    style JUDGE fill:#e6fdff,stroke:#0077b6,stroke-width:1.5px;
    style CLOUD fill:#fff6e0,stroke:#b07d1a,stroke-width:1.5px;
    style OUT fill:#ffeef4,stroke:#c9184a,stroke-width:1.5px;

    linkStyle default stroke:#6c757d,stroke-width:1.5px;
```

`summarize` uses whatever the local LLM produces on its own, plus — when RAG
is enabled — similar past incidents added to the prompt as context.

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
                     # model server's cold-load time (LM Studio JIT reload after idle).
                     # Connecting is capped separately at 5s, so a host that's
                     # powered off fails over in seconds, not after timeout_sec.
  # probe_timeout_sec: 5      # optional; health check before each call, 0 = off
  # breaker_failures: 2       # optional; circuit breaker, 0 = off
  # breaker_cooldown_sec: 120 # optional; see "Health checks and the circuit breaker"

telegram:
  bot_token: ""   # leave empty to disable the push
  chat_id: 0
```

`loki.endpoint` and `summarizer.endpoint` are required — the process exits
on startup if either is missing (unless `log_source.type` switches away
from Loki, see below). Everything else has a default or is optional. See
`deploy/config.docker.yaml` for the same shape with container-specific
comments (including the optional sections below).

### Environment variable overrides

Multiple `config.yaml` files (one per environment) already cover most of
what "12-factor config" would otherwise be for here — this repo doesn't
try to make every field environment-variable-driven. The one real gap
multiple YAML files don't close is keeping credentials *out of a file*
at all — for a `config.yaml` templated by CI, or mounted read-only from
a ConfigMap while the secret itself comes from a container
orchestrator's own secret store. A handful of credential fields can be
supplied this way instead, always winning over whatever `config.yaml`
has when set (an unset var never blanks out a value the file already
set):

| Field | Env var |
|---|---|
| `summarizer.api_key` | `VICTORIA_GATEWAY_SUMMARIZER_API_KEY` |
| `summarizer.fallbacks[N].api_key` | `VICTORIA_GATEWAY_SUMMARIZER_FALLBACK_<N>_API_KEY` (by position, e.g. `…_FALLBACK_0_API_KEY`) |
| `cloud.api_key` | `VICTORIA_GATEWAY_CLOUD_API_KEY` |
| `cloud_fallbacks[N].api_key` | `VICTORIA_GATEWAY_CLOUD_FALLBACK_<N>_API_KEY` |
| `escalation_targets.<name>.api_key` | `VICTORIA_GATEWAY_ESCALATION_<NAME>_API_KEY` (name upper-cased, non-alphanumerics → `_`, e.g. `aws-prod` → `…_AWS_PROD_API_KEY`; two names that map to the same variable, like `aws-prod` and `aws_prod`, are a startup error) |
| `judge.api_key` | `VICTORIA_GATEWAY_JUDGE_API_KEY` |
| `telegram.bot_token` | `VICTORIA_GATEWAY_TELEGRAM_BOT_TOKEN` |
| `rag.postgres_dsn` | `VICTORIA_GATEWAY_RAG_POSTGRES_DSN` |
| `rag.gitea.token` | `VICTORIA_GATEWAY_GITEA_TOKEN` |
| `rag.github.token` | `VICTORIA_GATEWAY_GITHUB_TOKEN` |
| `webhook_auth.password` | `VICTORIA_GATEWAY_WEBHOOK_AUTH_PASSWORD` |
| `webui_auth.password` | `VICTORIA_GATEWAY_WEBUI_AUTH_PASSWORD` |
| `alertmanager.password` | `VICTORIA_GATEWAY_ALERTMANAGER_PASSWORD` |

Each one only overrides a field inside a block `config.yaml` already
configures — `VICTORIA_GATEWAY_JUDGE_API_KEY` does nothing if there's no
`judge:` block at all, on purpose: an env var meant to hold a credential
for later use shouldn't be able to silently turn a whole feature on by
itself. (`VICTORIA_GATEWAY_CONFIG`, which picks *which* config.yaml to
load, is unrelated to this table and already existed.)

### Log source: Loki, CloudWatch, or GCP Cloud Logging

Loki is the default and requires no extra config beyond `loki.endpoint`
above. If your alerts' logs live in CloudWatch or GCP Cloud Logging
instead, `log_source` switches the backend `summarizeOne` fetches from —
`loki.lookback_sec`/`loki.limit` remain the general "how far back"/"how
many lines" knobs regardless of which one is active, since they aren't
Loki-specific, just historically homed on that block.

```yaml
log_source:
  type: "cloudwatch"   # "loki" (default), "cloudwatch", or "gcp_logging"
  cloudwatch:
    region: "us-east-1"
    log_group_names:
      - "/aws/lambda/your-function"
    # query_template: 'fields @timestamp, @message | filter host = "{{TERM}}"'
    # timeout_sec: 30   # optional; bounds the StartQuery/GetQueryResults poll loop
```

or, on GCP:

```yaml
log_source:
  type: "gcp_logging"
  gcp_logging:
    project_id: "your-gcp-project"
    # filter_template: 'jsonPayload.host="{{TERM}}"'
    # timeout_sec: 30
```

Both credentials paths use the standard SDK default chain — AWS's (env
vars, `~/.aws/credentials`, an EC2/ECS/EKS instance role, SSO) for
CloudWatch, Application Default Credentials (`gcloud auth
application-default login` locally, or the GCE/GKE/Cloud Run metadata
server in production) for GCP — deliberately, the same reasoning as
`cloud.provider: bedrock`'s: there's no static-key config field here on
purpose, so this doesn't become another place long-lived credentials end
up in `config.yaml`.

**`query_template`/`filter_template` matter more here than they might
look.** Neither CloudWatch Logs Insights nor GCP Cloud Logging has
anything like Loki's stream labels — there's no structured `{host="..."}`
selector to build. The default templates do a generic full-text search
(CloudWatch: `@message like /{{TERM}}/`; GCP: `SEARCH("{{TERM}}")`) against
whichever single term is most specific for the alert (pod, then
deployment/statefulset, then host — same precedence Loki's selector uses).
That's a reasonable starting point, not a claim it's the right query for
your log shape — structured JSON logs with a known field almost always do
better with a template that filters on that field directly, e.g.
`filter host = "{{TERM}}"` (CloudWatch) or `jsonPayload.host="{{TERM}}"`
(GCP). The token `{{TERM}}` must appear exactly once; it's a literal
string replace, not a format verb, so it's safe even if your own query
text contains `%` characters.

A blackbox probe's `instance` is a URL (`https://kmp.tw/health`), which
no log line or Loki stream is labeled with. When the host is a URL with a
scheme and a host, every log backend — Loki included — searches for just
its hostname instead (`kmp.tw`: no port, path or query; `[2001:db8::1]`
becomes `2001:db8::1`), and the switch is logged (`log search host
"https://kmp.tw/health" is a URL, searching for its hostname "kmp.tw"
instead`). Anything else, including `host:port` instances such as
`172.16.100.6:9100`, is searched for exactly as before. The alert is still
shown, filed and matched against past incidents under the full URL; only
the log query changes.

The term comes from alert labels, so it's treated as untrusted: a term
containing a quote, backslash, `/`, `|`, backtick, parenthesis,
whitespace or a control character is refused rather than spliced into
the query, since it could close the literal it sits in and append query
syntax of its own. Real host/pod names never contain these, and after the
URL reduction above a probe URL doesn't either; what's left is a label
that is odd even as a hostname (`http://a'b.example/`) or an injection
attempt. A refused term doesn't fail the alert: the query is
skipped, the alert is summarized without logs (the same as a query that
found nothing), and the skip is logged (`log query skipped on log source
"..."`, with the `refusing to search for ...` reason) and counted in
`victoria_gateway_log_query_refused_total{log_source=...}`. Any other log
query error still fails the alert as before. The
default CloudWatch template also regexp-escapes the term (a bare
`172.16.100.6` inside `/.../` would match `172x16y100z6` too); a custom
template gets it verbatim, because it may place it in a quoted string
instead. On GCP the template is always wrapped in parentheses after the
time-window clause, so an `OR` in your template can't widen the window.

CloudWatch Logs Insights queries are asynchronous (StartQuery, then poll
GetQueryResults) — `timeout_sec` bounds that whole poll loop, not just one
HTTP call. GCP Cloud Logging's `entries.list` is a single synchronous
call, so its `timeout_sec` is a plain HTTP timeout.

Setting `log_source.loki.endpoint` (a `loki:` block nested inside the
top-level `log_source`) is a startup error: per-source Loki endpoints are
a `log_sources` feature, and this mode always queries the top-level
`loki.endpoint`. Put the address there, or switch to `log_sources` with
**Hybrid cloud routing**.

`log_source` picks exactly one backend for the whole deployment. To use
several at once — on-prem Loki *and* CloudWatch *and* Cloud Logging, with
each alert queried against the one it actually lives in — see
**Hybrid cloud routing** below.

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

Request bodies are capped at 4 MiB regardless of auth (`413` above that).
A real Alertmanager payload is a few KB even for a large group; the cap
only keeps a misbehaving client from making the process buffer an
arbitrarily large body.

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

#### Slack, Teams and other chat webhooks

That fixed body is written for a consumer built against this service. A
chat product's incoming webhook wants its own schema: Slack's rejects a
body with no `text` (or `blocks`), and a Teams Workflows webhook expects
an Adaptive Card message. So a bare `type: webhook` channel pointed at
either one does not work. Give the channel a `body_template` (a Go
`text/template`) that renders the schema the endpoint expects:

```yaml
notifications:
  channels:
    - name: "slack-ops"
      type: webhook
      url: "https://hooks.slack.com/services/REPLACE/REPLACE/REPLACE"
      body_template: '{"text": {{json .Text}}}'
    - name: "teams-ops"
      type: webhook
      url: "REPLACE-workflows-webhook-url"
      body_template: |
        {"type":"message","attachments":[{"contentType":"application/vnd.microsoft.card.adaptive",
        "content":{"type":"AdaptiveCard","$schema":"http://adaptivecards.io/schemas/adaptive-card.json",
        "version":"1.4","body":[{"type":"TextBlock","text":{{json .Text}},"wrap":true}]}}]}
  routes:
    - default: true
      channels: ["slack-ops", "teams-ops"]
```

The template sees `.AlertName`, `.Host`, `.Summary`, `.AnalyzedBy`,
`.EscalatedTo`, `.Error`, `.PendingURL`, `.MitigationNote`, `.Similar`
(a list of `.Ref` / `.Date` / `.URL`), and `.Text`: the whole
notification pre-rendered as plain text, the same content as the Telegram
push without its HTML. Wrap anything free-form in `{{json ...}}`, which
quotes and escapes it: an LLM summary routinely contains quotes and
newlines, and pasted into a template bare it breaks the JSON. The
rendered body must be valid JSON. If it is not, the gateway refuses to
send it, logs the reason, and counts a failed push for that channel.
`body_template` is only valid on `type: webhook`, and a template that
does not parse stops the service at startup instead of failing at the
first alert.

What is and is not verified: the rendering is unit-tested against local
`httptest` servers (a Slack-shaped `{"text": ...}` body and a Teams
Adaptive Card envelope, including a summary with quotes, newlines and
angle brackets). **It has not been sent to a real Slack or Teams
endpoint**, and the two schemas above come from those products'
documentation, not from a live test, so try each with a throwaway
channel first. Two known rough edges: the text is not escaped for Slack's
own `&`, `<`, `>` control characters, and a Teams Adaptive Card has a
message size limit that a very long cloud analysis could exceed.

#### Getting alerts from something other than Alertmanager

`POST /webhook/alertmanager` is the only alert input, and it speaks the
Alertmanager webhook format. There is no CloudWatch, EventBridge or
Datadog receiver, and the intended way to add one is not another
receiver in this binary but a small translator in front of it, so the
gateway keeps one input contract. For a CloudWatch alarm that translator
is typically SNS → a Lambda function (or any small service) that
rebuilds the alarm as this payload and POSTs it:

```json
{
  "version": "4",
  "status": "firing",
  "alerts": [{
    "status": "firing",
    "labels": {"alertname": "HighErrorRate", "host": "checkout-api", "cloud": "aws"},
    "annotations": {"summary": "5xx rate above 5% for 5 minutes"},
    "startsAt": "2026-09-30T02:15:00Z",
    "fingerprint": "cloudwatch-HighErrorRate-checkout-api"
  }]
}
```

Things the translator has to get right, all of which the gateway checks
or depends on: `version` must be the string `"4"` and `alerts` must be
non-empty, or the request is rejected; `startsAt` must be RFC3339, since
it anchors the log query window; the labels must identify what to look
up (`host`/`instance`, or `namespace` + `pod`/`deployment`/`statefulset`),
because that is what the log query is built from; and a stable
`fingerprint` per alarm lets the gateway's dedup recognise a repeated
notification for the same episode (with none, every delivery is analyzed
again). Send `"status": "resolved"` when the alarm returns to OK so the
dedup entry is cleared. Labels are also what `hybrid_routes`,
`notifications.routes` and maintenance-window matchers match on, so the
`cloud: aws` label above is how such an alert reaches an AWS log source.

This translator is not part of the repository, and the payload above is
built from the format the gateway parses, not from a captured CloudWatch
event; the gateway side is covered by its own tests, the Lambda side is
yours to write and test.

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

There's also a Pending side, for records auto-captured right after an
alert fires but not yet confirmed by anyone:

- `GET /pending` — recent pending records, same `?limit=`/`?alertname=`/
  `?host=` filtering as `/incidents`. Rendered with a clear "unverified LLM
  guess" label and never mixed into the same table as confirmed records —
  a pending summary hasn't been checked by anyone yet.
  **Rows are grouped by `(alertname, host)`**, so one flapping check shows
  as a single row with a `×N` badge and how long it has been recurring,
  instead of N rows burying everything else. A real deployment had 40
  pending rows that were 4 groups: one `SSLCertExpiringSoon` for the same
  host was 35 of them, firing daily for nine days.
  The grouping key is `(alertname, host)` rather than the record's text on
  purpose — `summary` is regenerated by the LLM on every capture and
  `log_excerpt` covers a different time window each time, so hashing
  either would put every row in its own group and do nothing.
- `POST /pending/batch` — confirm every checked group in one go, all with
  the same resolution. The group's newest record stays searchable and the
  rest are marked `dup_of` it: still full records, still in `/incidents`,
  just not offered back to the LLM as N near-identical retrieval examples.
  Needs the `dup_of` column (`pkg/rag/migrate_0003_dup_of.sql`); without
  it the list still works and the page says how to enable the feature.
  Note that confirming a group asserts the other rows in it are the same
  thing — you've read the representative, not all N. The page says so.
- `GET /pending/{id}` — one pending record, with a form to confirm it:
  type in what it actually turned out to be, submit, and it moves to
  Confirmed (same as running `victoria-gateway note --id {id} --resolution
  "..."`). Confirming redirects nowhere special — the page re-renders
  showing the confirmation succeeded, with a link back to `/pending` — so
  this is usable end-to-end from a phone, matching the "on-call, on a
  phone, at night" assumption the dark theme was already built for.
  Submitting twice (a double tap, or a mail/IM client prefetching the
  link) is safe: only the first submission takes effect, the second sees
  "already confirmed" instead of silently overwriting the first answer.

Every Telegram push for a newly analyzed alert also links directly to
*that alert's own* `/pending/{id}` page (a "確認這筆" line, separate from
and above the "相似歷史事件" section, which only ever links to *other*,
already-confirmed incidents) — so confirming what you were just paged
about is one tap, not a detour through `/pending`'s list to find the
right row. Only appears when RAG capture actually produced a record
(RAG off, or the capture itself failed, means no link).

### What data this stores, and where it goes

Enabling RAG means every analyzed alert's log excerpt (up to 4,000
characters) ends up in several places: written into Postgres
(`incidents.log_excerpt`), sent to whichever LLM does the summarizing
(local and/or cloud), fed to the embedding endpoint, and rendered in
plain text on `/incidents/{id}` and `/pending/{id}` — both of which have
**no authentication by default** (see "Securing the web UI" below). What
the models write back (the summary, and a mitigation plan when an
escalation target produces one) is also stored, filed into the tracker
issue, and pushed in the notification, and it can quote whatever it was
shown.

If the Loki you're pointing this at is a real production instance, this
matters: production logs routinely contain passwords, tokens, and other
things you didn't mean to publish on an open port. `rag.mask_log_excerpt:
true` is the opt-in redaction for that. It is **off by default** (the
content of a log line is usually the root-cause signal — see its doc
comment in `pkg/config`), and when on it is applied at the two boundaries
text crosses, so each exit is covered:

| Where the text goes | Redacted when `mask_log_excerpt: true`? |
|---|---|
| Local summarizer prompt (log lines, alert `summary`/`description` annotations, retrieved past-incident context) | Yes |
| Cloud escalation prompt (same inputs) | Yes |
| Embedding endpoint input | Yes |
| Jev (the escalation judge, sent the local summary) | Yes, it is sent the redacted summary |
| Summary and mitigation plan the model writes back → notification, tracker issue, Postgres, `/incidents`, `/pending` | Yes |
| Stored `log_excerpt` in Postgres and the web pages | Yes |
| Alert **labels** (alertname, host, ...) | No — they identify the alert and the host |
| The incoming Alertmanager webhook body, Loki query text, and this process's own log output | No |
| Records stored **before** you turned the switch on | No — they stay as they were written (retrieved context from them is redacted on its way into a prompt, but the row itself is not rewritten) |

What "redacted" means is narrow: only `key=value` / `key: value`
assignments whose key looks like a secret (`password`, `passwd`, `pwd`,
`secret`, `token`, `api_key`, `access_key`, `credential`) and `Bearer
<token>` values are rewritten, each value replaced by its shape (digits
→ `0`, lowercase → `a`, uppercase → `A`), so `password=hunter2secret`
becomes `password=aaaaaa0aaaaaa` and everything else in the line, IPs and
ports included, is left readable. A secret with no such key next to it (a
bare API key in a stack trace, a connection string, a JSON field named
something else) is not caught. It is not a substitute for not logging
secrets in the first place.

Embeddings: turning the switch on changes the text that gets embedded,
but only the characters of a redacted value, so a new incident should
still land near its unredacted predecessors (not measured, the
difference is a few characters in a long text); you do not need to re-embed
old records, and the embedding-model drift check (which compares model
names) is unaffected.

### RAG has nothing to retrieve until something is Confirmed

`Search` only returns Confirmed records, and a fresh install starts with
zero of them. Until you've confirmed at least a few incidents (via
`/pending`, the confirm form, or `victoria-gateway note --alert-name ...`
for backfilling history from before RAG was enabled), the "相似歷史事件"
section simply won't have anything to show — RAG isn't broken, there's
just nothing in the store yet to be similar to.

### Turning repeat confirmations into suppression rule candidates

`victoria-gateway suppression-candidates` groups the confirmed-incident
history by (alertname, host) and prints every pair that's been confirmed
the same way at least `--min-count` times (default 3) — a signal that
alert keeps firing and getting manually waved off as known-noise. For
each candidate it prints a permanent Alertmanager `route` you could add
to stop being notified about it entirely, and a time-bounded `amtool
silence` command as the more conservative alternative. By default it only
ever prints — it never edits Alertmanager's config or calls its API, and
applying either option is entirely up to you. See `pkg/suppress`'s
package doc for exactly what the confirmation count does and doesn't
tell you (it's not the same thing as how often the alert actually fires).

`--apply-silences` can create the time-bounded silence option *for* you,
via Alertmanager's v2 silence API (see `pkg/alertmanager`) — never the
permanent route, and never a config file edit or reload. A silence is a
safe thing to automate because it self-expires; a wrong permanent route
doesn't, which is exactly why that option stays print-only. It's dry-run
by default (prints what it would create); add `--yes` to actually call the
API. It also skips any candidate that already has a matching active
silence, so re-running it doesn't pile up duplicates:

```bash
# Requires an `alertmanager:` block in config.yaml (see deploy/config.docker.yaml)
victoria-gateway suppression-candidates --min-count 5 --apply-silences          # dry run
victoria-gateway suppression-candidates --min-count 5 --apply-silences --yes     # actually create them
```

If audit logging is enabled (the default when RAG is on), every silence actually created this way is
recorded there (see "Audit log" below) — actor is `cli:$USER`.

### Securing the web UI

`/incidents`, `/pending`, and `/maintenance-windows` have no authentication
by default — the original assumption was a private network and a
read-only link to click from a notification. `/pending/{id}` now also
accepts a POST (the confirm form above), which changes that calculus:
anyone who can reach the port can now write a resolution, not just read
one. Set `webui_auth` in config.yaml (same shape as `webhook_auth`) to
require HTTP Basic Auth on all of these:

```yaml
webui_auth:
  username: ops
  password: "change-me"
```

**Basic Auth does not stop cross-site request forgery.** A browser that
has the credentials cached attaches them to a form POST coming from any
other site just as readily as to one from these pages, so on its own it
would let a link elsewhere confirm a pending incident in your name. Every
web UI route therefore also refuses writes (anything but GET/HEAD) that
the browser marks as cross-site: `Sec-Fetch-Site` must be `same-origin` or
`none`, or — for browsers that don't send it — `Origin`/`Referer` must
name this same host: the `Host` the request arrived with, or the scheme
and host of `rag.public_base_url`. The latter is what makes the pages
work behind a reverse proxy that rewrites `Host` (the browser's `Origin`
then names the public address, which victoria-gateway never sees as
`Host`), so set `public_base_url` to exactly the address people open
the pages at. `Origin: null` and any other host are still refused.
Refused requests get `403`. Requests carrying none of
those headers (curl, scripts calling `PUT /maintenance-windows`) aren't a
browser acting on someone's behalf and pass as before. This applies with
or without `webui_auth`.

This isn't multi-user or role-based — it's one shared credential, the
same tier of protection `webhook_auth` already offers the webhook
endpoint. If you need real SSO/OIDC, the auth check is a swappable
`AuthMiddleware` (see `cmd/victoria-gateway/auth.go`); basic auth is the
only implementation shipped today, but plugging in something else there
doesn't require touching any handler.

### Audit log

The one question none of the above answers: *who* changed the
maintenance windows, confirmed that pending incident, or applied that
suppression candidate as a silence — and when. It also answers the
automatic side of the same question: *why did this alert go to the cloud
(or not)*. Audit logging is **on by default whenever `rag.enabled: true`**
(it reuses that same Postgres database rather than adding a second
storage dependency; set `rag.audit_log: false` to opt out — an explicit
`audit_log: true` without `rag.enabled` is a config error). It records
these operations to an `audit_log` table (see
`pkg/audit` and `pkg/rag/schema.sql`), viewable at `GET /audit` —
authenticated by `webui_auth` the same as `/incidents` and `/pending`:

| Action | Recorded when |
|---|---|
| `maintenance_windows.replace` | `PUT /maintenance-windows` replaces the window set |
| `pending.confirm`, `pending.batch_confirm` | a pending incident (or a group) is confirmed on the web UI |
| `suppression.apply_silence` | `suppression-candidates --apply-silences --yes` creates a silence |
| `escalation.trigger` | an alert is handed to a cloud escalation target, with the reason, the target that answered, and `result=ok` / `result=failed error=...` |
| `escalation.rate_limited` | `escalation.max_per_hour` stopped an escalation that would otherwise have happened |

```yaml
rag:
  enabled: true
  # audit_log: false   # opt out; on by default when RAG is enabled
  # ... postgres_dsn / embedding_endpoint / embedding_model as usual
```

The actor recorded is the `webui_auth` username when that's configured,
the caller's remote IP otherwise (`ip:1.2.3.4`) — there's no real identity
system here, so an unauthenticated deployment gets the closest honest
substitute rather than an empty field. CLI-triggered entries (from
`suppression-candidates --apply-silences --yes`) record `cli:$USER`, and the
two `escalation.*` actions, which the alert pipeline decides on its own,
record `system`.
Analyzing an alert, notifying, and every other step of the core
webhook→Loki→LLM→notify path is not audited; an escalation is the one
automatic step recorded, because it spends money and sends alert context
to a third party, and the rate-limit entry is the only trace of the spend
guardrail having declined one. There is no per-alert "skip the guardrail"
switch to audit: `escalation.max_per_hour` is a config value, so changing
it is a config change, not a runtime operation this log can see.

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
lands first and the drain never happens. If a `gcp-cloud-assist` or
`aws-devops-agent` target (in `cloud`, `cloud_fallbacks` or
`escalation_targets`) can wait longer for its investigation than
`shutdown_grace_sec`, the startup banner prints a warning naming it.

## Triage: escalating a hard alert to a cloud model

The local model is fast and free, but it's a small quantized model — it can
misdiagnose alerts that need broader reasoning or turn up nothing when a
host's logs don't cover the real cause. Add a `cloud` block and victoria-gateway
can re-run the analysis against a stronger cloud model for alerts that need
it, while everything else still stays local. Gemini is the default provider
(`pkg/model.GeminiClient`); Anthropic (`pkg/model.AnthropicClient`), AWS
Bedrock (`pkg/model.BedrockClient`), Azure OpenAI
(`pkg/model.AzureOpenAIClient`), and Gemini on Vertex AI
(`pkg/model.VertexAIClient`) are also supported, so the three major
public clouds each have a native option, and `openai-compatible` covers
any self-hosted or proxied OpenAI-style server — see the dedicated
sections below for their extra setup:

```yaml
cloud:
  provider: "gemini"   # optional, defaults to "gemini"; also "anthropic", "bedrock", "azure-openai", "aws-devops-agent", "openai-compatible", "vertex-ai", "gcp-cloud-assist"
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
`cloud escalation failed` if that happens (or add `cloud_fallbacks`, see
**Fallbacks** below). Notifications and the JSON response also say which
target answered (`escalated_to`; in this single-`cloud` mode that's the
provider name, e.g. `bedrock`), without changing `analyzed_by`'s values.

Leaving `cloud` unset (the default) disables all of this — `escalation` with
no `cloud` configured is a startup error rather than a silent no-op. An
alert that asks to escalate when there's no `cloud` is logged
(`would escalate (...) but no cloud is configured`) and counted in
`victoria_gateway_escalation_no_target_total`. `provider: gemini` and
`anthropic` need an `api_key` (in the file or via the env var); without
one, the server refuses to start instead of every escalation failing
with 401 later. The reverse holds for `vertex-ai` and `gcp-cloud-assist`,
which authenticate with Application Default Credentials: an `api_key` on
such a block (typically a `VICTORIA_GATEWAY_*_API_KEY` left over from when
it was `provider: gemini`) is a startup error rather than silently
ignored. `openai-compatible` takes one if the server wants it. Only the server checks this: `victoria-gateway sync` and
`note` never call a cloud model, so they run fine from a shell that
doesn't have the `VICTORIA_GATEWAY_*_API_KEY` variables set.

### AWS Bedrock

`provider: "bedrock"` escalates to an Anthropic Claude model hosted on Amazon
Bedrock, for operators who want the escalation target to live inside AWS
rather than call Anthropic or Google directly:

```yaml
cloud:
  provider: "bedrock"
  region: "us-east-1"
  model: "us.anthropic.claude-haiku-4-5-20251001-v1:0"   # a cross-region inference profile ID, not the bare model ID -- see below
```

Unlike every other provider here, `pkg/model.BedrockClient` uses the AWS SDK
for Go v2 instead of a hand-rolled HTTP request — Bedrock's `InvokeModel`
requires AWS SigV4 request signing, not a bearer token, and reimplementing
that by hand is the wrong place to introduce a subtle bug. There's
deliberately no `api_key` field for this provider: credentials come from the
SDK's standard chain (environment variables, `~/.aws/credentials`, an
EC2/ECS/EKS instance role, SSO, ...), the same way any other AWS CLI/SDK tool
on the host already authenticates. The IAM identity used needs
`bedrock:InvokeModel` on the configured model (or inference profile — see
next paragraph).

⚠️ **Confirmed against a real account (2026-09-13)**: several current-generation
models reject a bare model ID for on-demand `InvokeModel` calls outright —

```
ValidationException: Invocation of model ID anthropic.claude-haiku-4-5-20251001-v1:0
with on-demand throughput isn't supported. Retry your request with the ID or ARN
of an inference profile that contains this model.
```

— and need the region-prefixed cross-region inference profile ID instead
(the `us.` prefix above, not a bare `anthropic.` one; list what's available
with `aws bedrock list-inference-profiles` or the SDK equivalent). Older
models like Claude 3 Haiku still accept the bare ID directly; which form a
given model needs isn't predictable from the ID alone. If `InvokeModel`
returns the error above, this is the fix. This provider has been verified
end-to-end in production — a real Alertmanager-triggered alert, escalated
through Bedrock, filed a Gitea incident with no errors.

### Azure OpenAI

`provider: "azure-openai"` escalates to a chat completions deployment on
Azure OpenAI / Microsoft Foundry:

```yaml
cloud:
  provider: "azure-openai"
  endpoint: "https://myresource.openai.azure.com"   # resource base URL, no path suffix
  deployment: "prod-gpt4o"   # the Azure *deployment* name, not the underlying model name
  api_key: "..."
  # api_version: "v1"   # optional; defaults to Microsoft's current documented default for this endpoint shape
```

`deployment` and `model` are easy to conflate but aren't the same thing in
Azure OpenAI: a deployment is a named, customer-created binding to a base
model (e.g. a deployment called `prod-gpt4o` backed by `gpt-4o`), and
requests address the deployment. `pkg/model.AzureOpenAIClient` targets the
current unified data-plane API
(`{endpoint}/openai/v1/chat/completions?api-version=...`) rather than the
older per-deployment-path API — if Microsoft's documented default
`api_version` ever looks stale, override it here rather than waiting on a
code change.

### Any OpenAI-compatible endpoint (vLLM, LiteLLM, OpenRouter, Ollama)

`provider: "openai-compatible"` sends the escalation to any server that
speaks `POST /v1/chat/completions` — a bigger model on a vLLM box, a
LiteLLM proxy in front of several vendors, OpenRouter, an Ollama host:

```yaml
cloud:
  provider: "openai-compatible"
  endpoint: "http://litellm.internal:4000"   # base URL; /v1/chat/completions is appended
  model: "gpt-4.1-mini"                      # whatever the server calls it
  api_key: "sk-..."                          # optional; no Authorization header when empty
  timeout_sec: 120                           # optional, default 60
```

`endpoint` is the part before `/v1`: `https://openrouter.ai/api` for
OpenRouter, `http://host:11434` for Ollama, `http://host:8000` for vLLM.
It's the same client the local summarizer uses (`pkg/model.OpenAIClient`),
so an error reply carries its HTTP status and body in the logged error,
and a host that is switched off fails within the 5-second connect
timeout instead of holding the alert for `timeout_sec`. `endpoint` and `model` are
required. The key can also come from the usual env var
(`VICTORIA_GATEWAY_CLOUD_API_KEY`, `…_ESCALATION_<NAME>_API_KEY`, ...).

### Vertex AI (Gemini on GCP, no API key)

`provider: "vertex-ai"` calls the same Gemini models through Vertex AI
instead of the Gemini Developer API. The project is billed, and access is
IAM, not an API key: credentials come from Application Default
Credentials — `GOOGLE_APPLICATION_CREDENTIALS`, `gcloud auth
application-default login`, or the GCE/GKE/Cloud Run metadata server (the
same chain the `gcp_logging` log source uses).

```yaml
cloud:
  provider: "vertex-ai"
  project: "my-gcp-project"
  model: "gemini-2.5-flash"
  location: "global"     # optional, default global; or a region such as us-central1, or us / eu
  timeout_sec: 60        # optional, default 60
```

`location` picks the endpoint, per Vertex AI's
[Locations](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/learn/locations)
page: `global` → `aiplatform.googleapis.com`, a region →
`{region}-aiplatform.googleapis.com`, `us`/`eu` →
`aiplatform.{us|eu}.rep.googleapis.com`. The default is `global` because
Google documents it as improving availability and reducing 429s. The
trade-off is data location: with `global` you
"can't control or know which region your ML processing requests are sent
to", so set a region (or `us`/`eu`) if that matters to you. The identity
needs `roles/aiplatform.user` on the project and the Vertex AI API
(`aiplatform.googleapis.com`) enabled. As with every escalation target,
any error — a 5xx or 429 (quota), but equally a 403 or a timeout — hands
the alert to the next target in the chain, if there is one; the HTTP
status only shows up in the error text that gets logged.
Verified 2026-09-27 against a real project on `global` with
`gemini-2.5-flash` and `gemini-3.5-flash`.

### Optional: escalating to AWS DevOps Agent instead of a chat completion

`provider: "aws-devops-agent"` escalates to
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
    mitigation_plan: false   # optional; true attaches a mitigation plan to the tracker issue (see below)
    mitigation_timeout_sec: 300  # optional, default 300
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

#### Mitigation plan in the tracker issue (`mitigation_plan`)

With `mitigation_plan: true`, a completed investigation is followed by
a request for AWS DevOps Agent's
[mitigation plan](https://docs.aws.amazon.com/devopsagent/latest/userguide/production-operations-autonomous-incident-response.html)
— its proposed Prepare / Pre-Validate / Apply / Post-Validate steps —
and the plan goes into the alert's tracker issue as its own section,
"建議處置（AWS DevOps Agent mitigation plan）", between the analysis and
the footer. The notification gets one line pointing at the issue, not
the plan (it's long, and Telegram caps a message at 4096 characters).

How it's fetched:

1. If the investigation already carries a mitigation summary (AWS
   documents inline mitigation proposals for alarm-triggered
   investigations), that's used and nothing else is started.
2. Otherwise `create_mitigation_plan` starts one — `UpdateBacklogTask`
   with `taskStatus: PENDING_START` on the completed task, the same
   thing the web app's "Generate mitigation plan" button does. This tool
   was added in v1.1.0 of the sample MCP server; an older install
   answers with an unknown-tool error and no plan is attached.
3. The task is polled until it completes again, and its executions are
   searched for the `mitigation_summary_md` journal record
   ([documented here](https://docs.aws.amazon.com/devopsagent/latest/userguide/configuring-integrations-and-knowledge-integrating-devops-agent-into-event-driven-applications-using-amazon-eventbridge-index.html#retrieving-an-investigation-or-mitigation-summary)).

Checked read-only against the real service on 2026-09-27: on
investigations whose plan had been generated earlier, the
`mitigation_summary_md` record sat in the investigation's own execution
(the task had no other execution), and this client read it back through
the sample server as a ~3,000-character Markdown plan (Action,
Reasoning, Execution Plan). Triggering a new plan was not exercised
live, since it starts a billed agent run.

The plan is only read. victoria-gateway never approves or applies it —
the issue says so, and acting on it stays a human decision.

It's best-effort: no plan, an API error (e.g. `AccessDeniedException`)
or running past `mitigation_timeout_sec` leaves the investigation's
analysis exactly as it would have been, logs why, and counts it in
`victoria_gateway_mitigation_plan_total{target,result="ok|none|error"}`.
If no tracker issue gets filed (no `rag.gitea`/`rag.github`, RAG off,
or filing failed), the plan is written to the log instead and the
notification line says so. RAG records don't store the plan.

**Off by default**, for cost and time: the mitigation run is a second
agent run billed per agent-second like the investigation
([$0.0083/agent-second](https://aws.amazon.com/devops-agent/pricing/);
AWS's sample documents 2-5 minutes per plan, so roughly $1-2.50 on top
of the investigation), and it adds those minutes to the escalation.
The startup banner's shutdown-grace warning counts
`mitigation_timeout_sec` on top of the 10-minute investigation limit.

IAM: the caller needs `aidevops:UpdateBacklogTask` ("approve a
mitigation plan" in the
[IAM reference](https://docs.aws.amazon.com/devopsagent/latest/userguide/aws-devops-agent-security-devops-agent-iam-permissions.html)),
`aidevops:GetBacklogTask`, `aidevops:ListExecutions` and
`aidevops:ListJournalRecords`. `AIDevOpsAgentFullAccess` already covers
all four.

Because the agent can only see AWS, a deployment that also watches
on-prem hosts shouldn't send *those* alerts to it. **Hybrid cloud
routing** (below) lets AWS alerts escalate to the DevOps Agent while
everything else keeps escalating to Gemini/Anthropic/Bedrock, from one
deployment.

### Optional: escalating to a Gemini Cloud Assist investigation (experimental)

> **Experimental.** Since 2026-04-10 Google only lets projects with a
> Premium Support contract, or access requested through the Google Cloud
> account team, create and run investigations; without it every run
> fails. The `investigations.create` and `revisions.run` methods this
> provider calls are also marked deprecated by Google, so it may stop
> working without notice. Details under "Before enabling it" below.

`provider: "gcp-cloud-assist"` is the GCP counterpart: it opens a
[Gemini Cloud Assist investigation](https://docs.cloud.google.com/cloud-assist/create-investigation)
in a GCP project, which reads that project's Cloud Logging, Cloud
Monitoring and resource configuration itself and comes back with ranked
hypotheses about the root cause.

```yaml
escalation_targets:
  gcp:
    provider: "gcp-cloud-assist"
    project: "my-gcp-project"
    poll_interval_sec: 15     # optional, default 15
    poll_timeout_sec: 270     # optional, default 270; the gateway gives up (and tries the next target) after this, counted from the start
    timeout_sec: 30           # optional, default 30; one HTTP call
```

It uses the REST API `geminicloudassist.googleapis.com/v1alpha` and the
same steps `gcloud beta gemini cloud-assist investigations create` does:
create an investigation in location `global` (the only one the API
serves; `location` may be omitted or `global`), with the project and the
whole escalation prompt — alert, RAG history, log excerpt — as the issue
description; run its revision; poll the returned operation; then read the
investigation. The hypotheses come back as Markdown, shown as-is like the
AWS DevOps Agent's report. Authentication is Application Default
Credentials, as for `vertex-ai`; there is no `api_key`.

Before enabling it:

- Enable the Gemini Cloud Assist API (`geminicloudassist.googleapis.com`)
  on the project, and grant the identity
  `roles/geminicloudassist.investigationCreator` (Google's own
  recommendation is to also have `logging.googleapis.com`,
  `monitoring.googleapis.com` and `cloudresourcemanager.googleapis.com`
  enabled so the investigation has something to look at). A 403 from the
  API comes back with a hint naming which of these is missing.
- **Access is restricted.** Google's docs: "As of April 10, 2026,
  creating, running, and editing investigations are only available to
  users that have a Premium Support contract or who have requested access
  through their account team." On a test project without either
  (2026-09-27), creating an investigation worked but every run failed
  within seconds with `an internal error has occurred (code 13)`; the
  error returned by this provider says to check that access.
- The v1alpha discovery document (revision 20260919) marks
  `investigations.create` and `revisions.run` as deprecated ("should only
  be created/run by the agent"). They're still what gcloud calls today,
  but this provider depends on them staying available.

Like the AWS DevOps Agent, an investigation takes minutes.
`poll_timeout_sec` is one budget for the whole escalation — creating the
investigation, starting it and waiting for it — and its default (270) is
kept under the default `shutdown_grace_sec` (300) so a restart doesn't cut
a waiting escalation short. If you raise it above `shutdown_grace_sec`,
the startup banner warns about it; raise `shutdown_grace_sec` (and the
compose `stop_grace_period`) with it. On a timeout the investigation
itself keeps running on GCP and can be opened in the console (its name is
in the result and in the timeout error). Route only alerts about that GCP
project to it, for example `escalation: [gcp, default]` on a
`matchers: {cloud: gcp}` route.

`escalation.max_per_hour` is an optional spend guardrail: at most this many
alerts escalate to `cloud` within a rolling hour, 0 (default) meaning
unlimited. Nothing else bounds cost if the local model's `escalate` signal
misfires broadly or `always_cloud` matches more alerts than intended — an
alert that hits the cap stays on the local result (logged, not failed)
instead of also calling `cloud`.

### Jev / TypeSafe AI escalation judge (optional)

**With no `judge.api_key` set, this feature is entirely disabled and
everything above behaves exactly as if it didn't exist** — this is an
opt-in extra, not a dependency. It calls out to
[TypeSafe AI](https://typesafe.ai)'s Jev model, a separate paid service
you'd need your own account for; nothing here bundles or requires it.

When enabled, every alert the escalation rules above did *not* already
decide to send to `cloud` gets one more, independent read: Jev is asked a
structured severity/escalation question about the local model's own
summary (see `pkg/judge`), and if its answer crosses a threshold, the
alert escalates too. This is additive only — it can turn a
non-escalating alert into an escalating one, but it can never suppress an
escalation the existing rules already decided on, and a failed or
unreachable Jev call (timeout, bad key, rate limit, ...) is logged and
falls back to whatever the existing rules already decided, never blocking
or delaying an alert that should escalate.

```yaml
judge:
  api_key: "..."               # TypeSafe AI System One API key; unset disables this entirely
  escalate_threshold: 0.70     # optional, defaults to 0.70 if unset — the order of magnitude
                                # TypeSafe's own llm_guardrails cookbook uses for an "action" threshold
```

`cmd/judge-eval` is a separate, standalone tool (not part of the running
service) for checking Jev's calibration against this deployment's own
past incident text before relying on it — see its `-h` output. Chinese-
language calibration isn't documented by TypeSafe; this repo's own
before-cutover check (repeat-calling the same input several times to
separate genuine model variance from ordinary record-to-record text
differences) is the standard this threshold should be re-checked against
if you significantly change your alert mix or summarizer prompt.

## Fallbacks: when a model is down

Both the local summarizer and escalation can name backups. For the
local summarizer, only an unavailable backend triggers the next one —
connection refused or reset, timeout, a 5xx, or a 429. Any other 4xx
(401, 400, 404, ...) means the request itself is wrong — a bad
`api_key`, model name or endpoint path — so the alert fails right there
with that error: no fallback, no cloud, since either would only hide a
configuration mistake behind extra load or cloud spend. A model that
answers, even with something that isn't valid JSON, has answered: its
reply is used as-is, the same as without fallbacks. (A model that
returns an empty reply twice in a row isn't treated as unavailable
either; that alert fails as it always did.)

### Local summarizer fallbacks

```yaml
summarizer:
  endpoint: "http://llm-a:8091"     # primary, e.g. MLX
  model: "primary-model"
  timeout_sec: 180
  fallbacks:                         # tried in order
    - endpoint: "http://llm-b:1234"  # e.g. LM Studio on another box
      model: "backup-model"
      timeout_sec: 180
      # api_key: "..."   # or VICTORIA_GATEWAY_SUMMARIZER_FALLBACK_0_API_KEY
```

Each switch is logged (`aiops: summarizer failed for alert "X": ...;
trying summarizer.fallbacks[0]`, then `summarized by fallback
summarizer.fallbacks[0]`). A fallback can't have `fallbacks` of its own.

**If every local model is down**, the alert isn't failed straight away:
when its route has an escalation target (the `cloud` block, or the
hybrid route's `escalation`), it's handed to that target with reason
`local LLM unavailable`. This still counts against
`escalation.max_per_hour`, and skips Jev (Jev judges the local summary,
and there isn't one). Only when there's no target, the hourly budget is
spent, or the escalation fails too does the alert fail — and then the
error names both halves, e.g. `summarize: all 2 local summarizers
failed: ...; cloud escalation (local LLM unavailable) also failed: ...`.
With no escalation target configured, the error text is exactly what it
was before fallbacks existed.

If an escalation target is configured, also set `escalation.max_per_hour`:
while the local model is down every alert becomes a cloud call, and
this is what caps that bill during an outage or an alert storm.

### Health checks and the circuit breaker

The 5-second connect cap only helps when the host is gone. A Mac that
went to sleep, or a model server that's wedged, still accepts the TCP
connection and then never sends a response header — without anything
else, every alert would wait out the whole `timeout_sec` (180s in the
example above) before moving on, and each fallback adds its own wait on
top. Two things, both on by default, stop that:

- **Health check.** Before every chat call, each backend gets a
  `GET /v1/models` bounded by `probe_timeout_sec` (default 5). If that
  times out, can't connect, or gets a 5xx or 429, the backend is skipped
  as unavailable — the chat isn't sent — and the next fallback, or the
  cloud, takes the alert. Any other answer, such as a 404 from a server
  that doesn't implement `/v1/models` or a 401, says nothing about
  whether chat works, so chat goes ahead (logged once per backend).
  `/v1/models` is a static list on LM Studio, Ollama and the MLX shim and
  answers while a generation is running, so the check adds milliseconds
  on a healthy host.
- **Circuit breaker.** After `breaker_failures` (default 2) unavailable
  failures in a row — health check or chat — the backend's breaker opens
  and for `breaker_cooldown_sec` (default 120) it's skipped outright, with
  no health check and no chat. When the cooldown ends, the next alert is
  let through as a trial while concurrent alerts keep skipping: if it
  gets an answer the breaker closes, otherwise it opens for another
  cooldown. Only unavailable failures count; any answer, including one
  that isn't valid JSON or a 4xx configuration error, resets the count.

```yaml
summarizer:
  endpoint: "http://llm-a:8091"
  timeout_sec: 180
  probe_timeout_sec: 5         # 0 turns the health check off
  breaker_failures: 2          # 0 turns the breaker off
  breaker_cooldown_sec: 120    # 0 turns the breaker off too
  fallbacks:
    - endpoint: "http://llm-b:1234"
      probe_timeout_sec: 10    # a fallback inherits the three values above
                               # unless it sets its own
```

A config that doesn't mention these fields gets the defaults. Setting
`probe_timeout_sec: 0` and `breaker_failures: 0` gives exactly the
behavior from before they existed. Negative values are rejected at
startup.

When every local backend is skipped — each one's health check failed or
its breaker is open — the alert goes down the same **If every local
model is down** path as before: escalated with reason `local LLM
unavailable` when there's a target, failed otherwise. Breaker changes
are logged with the backend name (`circuit breaker for summarizer
opened after 2 consecutive unavailable failure(s); skipping it for
2m0s`, `... half-open after 2m0s; letting one alert through to test it`,
`... closed; it is answering again`, `... re-opened: the trial alert
failed too`), and `/metrics` has
`victoria_gateway_local_llm_breaker_open{backend=...}` (1 while open or
testing, 0 while closed) and
`victoria_gateway_local_llm_skipped_total{backend=...,reason="probe"|"breaker"}`.
Breaker state lives in memory, so a restart starts every backend closed.

### Escalation target fallbacks

Single-`cloud` configs add `cloud_fallbacks`, a list of blocks with the
same shape as `cloud`:

```yaml
cloud:
  provider: bedrock
  region: us-east-1
  model: us.anthropic.claude-haiku-4-5-20251001-v1:0
cloud_fallbacks:
  - provider: bedrock
    region: us-west-2
    model: us.anthropic.claude-haiku-4-5-20251001-v1:0
  - provider: anthropic
    model: claude-haiku-4-5
    # api_key via VICTORIA_GATEWAY_CLOUD_FALLBACK_1_API_KEY
```

With `hybrid_routes`, list targets in the route instead
(`escalation: [aws, default]`); `cloud_fallbacks` alongside
`hybrid_routes` is rejected.

Targets are tried in order until one answers. Unlike the local
summarizer, *any* failure moves on to the next target — a 401, 403 or
bad model name as much as a 5xx, 429 or timeout; the status code only
changes the error text. Each failure is logged
(`cloud escalation to "aws" failed ...`, then `falling back to escalation
target "default"`) and counted in
`victoria_gateway_escalation_failures_total{target=...}`; if every target
fails, the local result is used, as before. However many targets one
alert tries, it uses **one** slot of `escalation.max_per_hour` — a failed
call isn't the spend that budget exists to bound.

## Hybrid cloud routing: several log backends and escalation targets in one deployment

`log_source` and `cloud` each pick exactly one thing for the whole
deployment. That's fine when everything you watch lives in one place, but
it breaks down as soon as a cloud account is meant to be an extension of
the machine room rather than a separate world: an alert about an on-prem
host has its logs in Loki, an alert about a Lambda function has them in
CloudWatch, and only the latter makes sense to hand to AWS DevOps Agent,
which can see the AWS account and nothing else.

`hybrid_routes` routes each alert by its labels to a named log source and
a named escalation target:

```yaml
loki:
  endpoint: "http://loki:3100"   # default for any type-loki source below
  lookback_sec: 300              # still global, for every backend
  limit: 200

log_sources:
  onprem: {type: loki}
  site-b:
    type: loki
    loki: {endpoint: "http://loki-site-b:3100"}   # a second Loki, e.g. another site
  aws:
    type: cloudwatch
    cloudwatch: {region: us-east-1, log_group_names: ["/aws/lambda/checkout"]}
  gcp:
    type: gcp_logging
    gcp_logging: {project_id: my-project}

escalation_targets:
  default: {provider: gemini, model: gemini-2.5-flash}   # api_key via VICTORIA_GATEWAY_ESCALATION_DEFAULT_API_KEY
  aws:
    provider: aws-devops-agent
    aws_devops_agent: {user_id: "...", space_id: "..."}

hybrid_routes:
  - matchers: {cloud: aws}
    log_source: aws
    escalation: aws
  - matchers: {cloud: gcp}
    log_source: gcp
    escalation: default
  - matchers: {site: b}
    log_source: site-b
    escalation: default
  - default: true
    log_source: onprem
    escalation: default
```

How it's evaluated:

- Routes are checked in config order; the first route whose matchers all
  match the alert's labels wins. Matchers use the same glob syntax as
  maintenance windows and `notifications.routes` (`env: "prod-*"`).
- A `default: true` route is required and must be last — every alert needs
  somewhere to get its logs from.
- `escalation` may be left out of a route: alerts on it never escalate,
  and the local result is final. It may also be a list,
  `escalation: [aws, default]` — later targets are tried only when an
  earlier one fails (see **Fallbacks** below).
- Routes that share a log source or escalation target name share one
  client.
- Where the `cloud` label comes from is up to you — typically a static
  label on the Prometheus/CloudWatch-exporter scrape job or the
  Alertmanager route that forwards those alerts.

What stays global: `loki.lookback_sec`/`loki.limit`, `escalation.always_cloud`
(an alert on a route without an escalation target still can't escalate —
it's logged as `would escalate (...) but route "..." has no escalation
target`, and startup prints a warning when `always_cloud` is set and some
route has no target), `escalation.max_per_hour` (one budget shared by
every target), and the Jev judge. `analyzed_by` in results and RAG
records is still `local`/`cloud`; which target answered is in the
separate `escalated_to` field of notifications, the webhook JSON and the
tracker issue (not in RAG records — that would need a schema migration
for a value the issue already carries). Which route, log source and
escalation target an alert used is logged per alert (`routed via
hybrid_routes[0] → log source "aws", escalation "aws"`), and the startup
line lists every configured source and target.

**Routing labels are an attack surface.** Whoever can put labels on an
alert decides which log backend is queried and which escalation target
is called — and anyone who can reach `/webhook/alertmanager` can send an
alert with any labels they like. If a route sends alerts to a costly or
powerful target (AWS DevOps Agent starts a real investigation in your
AWS account), turn on `webhook_auth` so only Alertmanager can post, set
`escalation.max_per_hour` so a flood of crafted alerts can't run up an
unbounded bill, and prefer matching on labels your own scrape config or
Alertmanager route sets over labels an application can emit itself.
Matcher globs are checked at startup (an unterminated `[` class is an
error, not a route that silently never matches).

**Relationship to the single-backend config:** with `hybrid_routes` unset,
`log_source`/`cloud` work exactly as before — existing configs need no
change. `hybrid_routes` and the legacy `log_source` or `cloud` block can't
be set together; `Validate` rejects that instead of silently picking one,
so there's never a question of which one is actually in effect. It also
rejects routes that reference an undefined name, and `log_sources`/
`escalation_targets` entries no route uses (almost always a typo'd route
name that would otherwise fall through to the default route unnoticed).

Shared incident memory falls out of this for free: one deployment means
one RAG store, so an incident confirmed on an AWS alert is retrievable
context for the next on-prem alert that looks like it, and vice versa —
including AWS DevOps Agent's conclusions, which are captured like any
other escalation result.

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

That leaves two independent ways to confirm a record — the `/pending/{id}`
web form, or closing the linked issue yourself — and by default they don't
talk to each other: confirming via the web form only writes to the
database, the issue stays open until `sync` or someone notices. Set
`rag.close_issue_on_web_confirm: true` to keep them in sync in that
direction too: confirming via `/pending/{id}` then also closes the linked
issue, posting the typed-in resolution as the closing comment. Off by
default — some setups deliberately want the issue tracker to stay the
single source of truth (e.g. an existing on-call process built around
"closing the issue is the confirmation"), and this toggle exists so the web
form doesn't quietly become a second one behind their back. A failure to
reach the tracker here is logged, not surfaced to whoever just confirmed
(they have no tracker credentials to act on it anyway) — the database
confirmation already succeeded and is never rolled back for it; the issue
just stays open until the next `sync` tick or a manual close.

```yaml
rag:
  close_issue_on_web_confirm: true   # optional, defaults to false
```

**Real-time sync via webhook.** `sync` only runs when its cron job fires —
however often that's scheduled (commonly every 30 minutes), that's the
worst-case lag between closing an issue and the database/notifications/
Grafana reflecting it. Configuring a
`webhook_secret` under `gitea:` or `github:` turns on an endpoint that
resyncs the one issue that just closed, immediately:

```yaml
rag:
  gitea:
    # ...endpoint/token/owner/repo as above...
    webhook_secret: "..."   # enables POST /webhook/gitea-issues
  # or, symmetrically:
  # github:
  #   webhook_secret: "..."   # enables POST /webhook/github-issues
```

Each endpoint is disabled (404s unconditionally) unless its `webhook_secret`
is set, and verifies every request against it before doing anything else:
Gitea's `X-Gitea-Signature` header (hex HMAC-SHA256 of the raw body) or
GitHub's `X-Hub-Signature-256` header (`sha256=` + hex HMAC-SHA256),
matching each tracker's own documented scheme. Only the `issues` event's
`closed` action triggers a resync; anything else (opened, edited, reopened,
a non-issue event) is accepted and ignored. Register it on the *issues*
repo (`rag.gitea.repo`/`rag.github.repo` — the dedicated repo issues get
filed to, not the code repo) pointing at
`http://<this-host>:<port>/webhook/gitea-issues` (or `/webhook/github-issues`),
subscribed to the "Issues" event, with the same secret configured above.

`sync` isn't replaced by this — it keeps running as the fallback for any
delivery the webhook missed (the service was down, the request timed out,
network hiccup) or for issues closed before the webhook was ever set up.
If both `close_issue_on_web_confirm` and a webhook are enabled, confirming
via the web form closes the issue, which then fires the webhook back at
this same service — handled as an expected no-op (the record is already
Confirmed by then), not an error, so the two features don't chase each
other in a loop.

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

### Grafana dashboard

`deploy/grafana-dashboard.json` is a 3-panel dashboard over the `incidents`
table — Confirmed count, Pending count, and a sortable/filterable table of
every incident (alert, host, status, linked issue number, log excerpt,
summary, resolution, timestamps). Requires `rag.enabled` (it reads straight
from Postgres, not through victoria-gateway's own API) and a Grafana
Postgres datasource pointed at that same database.

Import it either through Grafana's UI (Dashboards → New → Import, paste the
file's contents or upload it) or by dropping it into a
[dashboard provisioning](https://grafana.com/docs/grafana/latest/administration/provisioning/#dashboards)
directory for GitOps-style setups. Either way, the datasource UID the
dashboard's panels reference (`victoria-gateway-postgres`) needs to match an
actual datasource in your Grafana — either name/configure your Postgres
datasource with that UID, or re-point each panel's datasource after
importing.

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
container healthcheck or a quick "is this running" check. A container
healthcheck only tells the container runtime *on the same host* though —
if the whole host goes down, nothing local is left to notice. See
`deploy/external-healthcheck/` for a Kubernetes CronJob that checks
`/healthz` from a genuinely separate machine and pushes a Telegram alert
if it can't reach it: victoria-gateway watches other services for silent
failure, which makes it worth having something watch it back.

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

A few families carry a label saying where the work went. The names
didn't change when the labels were added, so a query that sums over the
family (`sum(rate(victoria_gateway_escalations_total[1h]))`) gives the
same number it always did; until the first sample, each family shows one
unlabeled `0`.

| Metric | Label | Value in single-backend (non-hybrid) mode |
|---|---|---|
| `victoria_gateway_alerts_total`, `victoria_gateway_alerts_error_total` | `route` (`hybrid_routes[N]` or `default`) | `legacy` |
| `victoria_gateway_escalations_total`, `victoria_gateway_escalation_failures_total`, `victoria_gateway_escalation_rate_limited_total` | `target` (escalation target name) | provider, e.g. `bedrock`; a fallback is `bedrock (cloud_fallbacks[0])` |
| `victoria_gateway_escalation_no_target_total` | `route` | `legacy` |
| `victoria_gateway_loki_query_duration_seconds_sum`/`_count` (every log backend, despite the name) | `log_source` (log source name) | the type: `loki`, `cloudwatch`, `gcp_logging` |
| `victoria_gateway_log_query_refused_total` (alert summarized without logs because its search term was refused) | `log_source` | as above |
| `victoria_gateway_cloud_llm_duration_seconds_sum`/`_count` | `target` | as for `escalations_total` |
| `victoria_gateway_local_llm_skipped_total` (local backend skipped without a chat call) | `backend` (`summarizer` or `summarizer.fallbacks[N]`), `reason` (`probe` or `breaker`) | same |
| `victoria_gateway_local_llm_breaker_open` (gauge, 1 = open) | `backend` | same; one series per backend with a breaker from startup, none if every breaker is off |
| `victoria_gateway_mitigation_plan_total` (mitigation plans requested alongside a successful escalation; only `aws-devops-agent` with `mitigation_plan: true` today) | `target`, `result` (`ok`, `none`, `error`) | as for `escalations_total` |

Label values are written with the text format's own escaping (only `\`,
`"` and newline), so a route, target or channel name comes back from a
scrape exactly as configured.

Or in a container — same image either way, two deployment shapes on top
of it depending on where the rest of your monitoring stack already runs:

**Docker Compose** — for a single host already running
Loki/Prometheus/Alertmanager in one `docker-compose.yml`, which is how
the reference deployment runs it:

```bash
docker build -t victoria-gateway:latest .
```

`deploy/docker-compose.snippet.yml` has the service block to add to an
existing docker-compose stack (same network as Loki/Prometheus/Alertmanager).
`deploy/alertmanager_receiver_example.md` covers wiring it into an existing
Alertmanager route as an additive second webhook target.

**Kubernetes** — for a cluster instead of a single host: `deploy/k8s/`
has a Deployment/Service/Secret (fixed at 1 replica — see that
directory's README for why), a direct translation of the same
`config.yaml` shape, not a different way of configuring the service.

Either path uses the same binary, the same `config.yaml`, and every
feature above works identically regardless of which one runs it — pick
whichever matches where you already run things.

## Status

**Kubernetes deployment** (`deploy/k8s/`): verified end to end on a real
6-node cluster as of 2026-09-22 — image built and pushed to a private
registry, pulled and run successfully on every node (including working
through a `x509: certificate signed by unknown authority` failure on
the 5 nodes that didn't yet trust that registry's CA, fixed via each
node's container-runtime registry config), Secret-mounted config,
`/healthz` reachable through the Service, and a real synthetic alert
processed correctly end to end (Loki query, local LLM summary) through
a port-forward. **RAG verified separately, same day**: a second
deployment with `rag.enabled: true` (same Postgres/pgvector store and
embedding endpoint the docker-compose deployment uses) captured a real
alert as Pending, retrieved and correctly referenced two genuinely
existing Confirmed incidents in the generated summary (proving
embedding + pgvector search + prompt grounding all work cross-subnet
from inside the cluster, not just connectivity), and the web confirm
form (`POST /pending/{id}`) moved it to Confirmed and onto `/incidents`
correctly. Both deployments torn down after verification — this
deployment shape is new and unproven in long-running production, unlike
the docker-compose
path below.

Running in production against a home Alertmanager/Loki/LM Studio stack,
with CI (gofmt/build/vet/`go test -race`/golangci-lint) on every push and
PR. Every feature above — Triage, RAG capture/retrieval, the Gitea/GitHub
tracker integrations, webhook auth, fingerprint dedup, concurrent
multi-alert processing, maintenance windows — has been exercised against
real alerts on that deployment, not just unit tests.

**Jev / TypeSafe AI escalation judge**: live in production (not dry-run)
as of 2026-09-21, `judge.escalate_threshold` at the 0.70 default. Before
enabling it for real, this deployment's own past incident text (2
Confirmed + several Pending records) was run through `cmd/judge-eval`,
including repeat-calling identical text 8 times each on the two most
divergent-looking records to separate genuine model variance from
ordinary record-to-record differences — severity/escalate_probability
came back highly reproducible (σ ≈ 0.03–0.06 on the 0-3 severity scale)
once the same exact input was held fixed, so the spread seen across
different records reflects real content differences, not an unstable
judge. Verified end-to-end against the real deployment post-cutover: a
test alert through the live webhook produced a `judge:` log line with
real severity/confidence/escalate_probability, correctly identifying an
explicitly-test-labeled alert as severity 0 with high confidence (the
same signal a human would read off it), OR-merged with the existing
local/always_cloud signal, captured to RAG, and filed to Gitea — test
artifacts (the pending record and the issue) were cleaned up afterward.

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
