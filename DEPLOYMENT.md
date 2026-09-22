# Deployment checklist

Step-by-step install for a fresh host or cluster. This is the "do these in
order and it runs" companion to `README.md`, which is where the reasoning
behind each piece lives; where a step has a non-obvious *why*, this file
points at the README section instead of restating it. The container path
is assumed throughout — building from source instead is a one-liner
covered in README's "Running" section.

Two deployment shapes, same binary and same `config.yaml`: **Docker
Compose** (section 2A), the reference deployment (the author's home
monitoring host, running Loki/Prometheus/Alertmanager in one
`docker-compose.yml`) and the more battle-tested path — or **Kubernetes**
(section 2B), verified end to end but newer and less proven in
long-running production (see README's "Status"). Pick one; everything
from section 3 onward applies to either, with a short note wherever the
exact command differs.

Sections 1 and 2 are the whole minimal install. Everything from section 3
on is optional and can be added later, one at a time, without redoing the
earlier steps.

## 1. Prerequisites

- [ ] **Docker Compose** (a host already running the monitoring stack) **or
      a Kubernetes cluster** — whichever you're deploying to. Building from
      source instead of using a container needs Go 1.25+, per `go.mod`;
      the Dockerfile builds with `golang:1.25` so the container path has no
      host Go requirement either way.
- [ ] **Alertmanager already running** and reachable from that host or
      cluster. This service is an additive webhook receiver; it doesn't
      replace anything in your existing routing.
- [ ] **Loki** reachable from the container, or CloudWatch / GCP Cloud
      Logging credentials on the host if your logs live there instead (see
      README's "Log source: Loki, CloudWatch, or GCP Cloud Logging"; the
      steps below assume Loki).
- [ ] **An OpenAI-compatible LLM endpoint** for `summarizer`: a local
      server (LM Studio, Ollama, vLLM) or a cloud one that accepts a bearer
      key. Note its base URL and the model name it serves.
- [ ] **A Telegram bot token and chat ID** (README, "Getting a Telegram bot
      token and chat ID"). Without a push destination the summary is
      computed and then discarded, so don't skip this.
- [ ] *Only if enabling RAG (section 3):* Postgres with the pgvector
      extension, plus an OpenAI-compatible `/v1/embeddings` endpoint
      serving an embedding model (the same local server as `summarizer` is
      fine if it also has one loaded). `deploy/rag-quickstart/` stands up
      the Postgres part for you.

## 2. Minimal install

No RAG, no auth, no escalation. Just webhook in, summary out to Telegram.

## 2A. Docker Compose

### 2A.1 Build the image

On the monitoring host, from the repo root:

```bash
docker build -t victoria-gateway:latest .
```

### 2A.2 Create the config

Copy the annotated template next to your stack's `docker-compose.yml`. The
compose snippet in the next step mounts it from `./victoria-gateway/config.yaml`
relative to that file:

```bash
mkdir -p ./victoria-gateway
cp deploy/config.docker.yaml ./victoria-gateway/config.yaml
```

Fill in every `REPLACE` marker in the non-commented blocks. For the minimal
install that is exactly these:

| Field | Value |
|---|---|
| `loki.endpoint` | Loki's service name on the shared docker network, e.g. `http://loki:3100` (the template default) |
| `summarizer.endpoint` | Your LLM server's base URL. If it runs on another machine, use its LAN IP; a container/service name only resolves on the same docker network |
| `summarizer.model` | The model name that server exposes |
| `telegram.bot_token` / `telegram.chat_id` | From the prerequisites |

Leave every commented-out block (`webhook_auth`, `cloud`, `rag`, ...)
commented for now. The template's `webhook_async: true` and
`summarizer.timeout_sec: 180` are already the right values for a real
Alertmanager in front and a local model that cold-loads after idle; see
README's "Async webhook mode and graceful shutdown" and the `timeout_sec`
note under "Config" for why.

`loki.endpoint` and `summarizer.endpoint` are the two fields the process
refuses to start without.

### 2A.3 Add the compose service

Paste the service block from `deploy/docker-compose.snippet.yml` into the
existing `docker-compose.yml` (don't start a separate compose file; the
stack is managed as one). Two things to check against your stack:

- `networks: [monitor]` must be the network Loki/Prometheus/Alertmanager
  are actually on. Rename it if yours is called something else.
- `stop_grace_period: 5m30s` is deliberate: it must stay `>=`
  `shutdown_grace_sec` (default 300s) or Docker's 10s SIGKILL cuts off the
  drain of in-flight analyses on every redeploy.

The `ports: ["8090:8090"]` mapping is only there so you can curl the
service from the host for the tests below; Alertmanager reaches it by
service name over the docker network and doesn't need it. Drop it later if
you don't want the port on the host.

### 2A.4 Start it

```bash
docker compose up -d victoria-gateway
docker compose logs -f victoria-gateway
```

A config error (missing endpoint, malformed block) exits immediately with a
`❌` line in the logs; fix the file and `up -d` again.

### 2A.5 Verify: health

```bash
curl -i http://localhost:8090/healthz
```

Expect `HTTP/1.1 200 OK` with body `ok`.

### 2A.6 Verify: a synthetic alert

POST a minimal Alertmanager-shaped payload. The alert must carry a `host`
or `instance` label (or `namespace`+`pod` for a Kubernetes alert), because
that's what scopes the Loki query; use a hostname that actually has logs in
Loki so the LLM has something to read:

```bash
curl -i -X POST http://localhost:8090/webhook/alertmanager \
  -H "Content-Type: application/json" \
  -d '{
    "version": "4",
    "status": "firing",
    "receiver": "test",
    "alerts": [
      {
        "status": "firing",
        "labels": {"alertname": "DeploymentSmokeTest", "host": "REPLACE-a-host-loki-knows", "severity": "warning"},
        "annotations": {"summary": "victoria-gateway deployment smoke test"},
        "startsAt": "2026-01-01T00:00:00Z",
        "endsAt": "0001-01-01T00:00:00Z",
        "fingerprint": "deploy-smoke-test-1"
      }
    ]
  }'
```

With the template's `webhook_async: true` the response is
`202 Accepted` with `{"status":"accepted","accepted":1}`, and the actual
analysis runs in the background: watch `docker compose logs -f
victoria-gateway` for the Loki query, the LLM call, and the Telegram push,
and confirm the summary lands in the Telegram chat. If you'd rather see
the full result in the HTTP body, set `webhook_async: false` temporarily
and the same POST returns `200` with `{"results":[...]}` once the analysis
finishes.

Re-sending the same payload is deduplicated by `fingerprint` (always on);
change the fingerprint to trigger a fresh analysis, or send the same alert
with `"status": "resolved"` to clear the dedup entry.

### 2A.7 Wire it into Alertmanager

Add it as a **second** `webhook_configs` entry on a receiver your routes
already use, then reload. Full example and notes in
`deploy/alertmanager_receiver_example.md`:

```yaml
receivers:
  - name: 'yourExistingReceiver'
    webhook_configs:
      - send_resolved: true
        url: 'http://<your-existing-webhook-target>'
      - send_resolved: true
        url: 'http://victoria-gateway:8090/webhook/alertmanager'
```

```bash
curl -X POST http://<alertmanager-host>:9093/-/reload
```

Keep `send_resolved: true`: `resolved` deliveries are never analyzed, they
only clear the dedup entry so the next real firing isn't mistaken for a
duplicate. Also check the route's `repeat_interval`; a short one re-runs
the LLM and re-pushes to Telegram on every repeat, not just the first
firing.

That's the minimal install done (Docker Compose path). The next real
alert through that receiver will produce a Telegram summary. Skip ahead
to section 3.

## 2B. Kubernetes

Full steps, including the registry-trust gotcha that's easy to miss, are
in `deploy/k8s/README.md` — this section is the condensed version for
following along here.

### 2B.1 Build and push the image

Unlike Compose, every node your workload might land on needs to be able
to pull the image — a `docker build` on your own machine alone isn't
enough unless every node can load it directly (single-node clusters
only):

```bash
docker build --platform linux/amd64 -t <your-registry>/victoria-gateway:latest .   # match your nodes' actual architecture
docker push <your-registry>/victoria-gateway:latest
```

If `<your-registry>` is self-hosted or uses a certificate your cluster's
container runtime doesn't already trust, **test a real pull from the
cluster before going further** — a `docker push` succeeding only proves
your own machine trusts it:

```bash
kubectl run pull-test --image=<your-registry>/victoria-gateway:latest --restart=Never -- /usr/local/bin/victoria-gateway --help
kubectl get pod pull-test   # watch for ImagePullBackOff
kubectl delete pod pull-test --now
```

A failure here (commonly `x509: certificate signed by unknown
authority`) means your container runtime's registry config needs the
registry's CA added — node-local, runtime-specific, see
`deploy/k8s/README.md`'s "Self-signed or private registries" section.
This is not optional to check: a manifest can apply cleanly and still
sit in `ImagePullBackOff` forever if this step is skipped.

### 2B.2 Create the config and apply the manifests

Same `config.yaml` as the Compose path (`deploy/config.docker.yaml`
template, same required fields) — delivered as a Secret instead of a
bind-mounted file:

```bash
cd deploy/k8s
kubectl apply -f namespace.yaml
kubectl create secret generic victoria-gateway-config -n victoria-gateway --from-file=config.yaml=./config.yaml
```

Edit `deployment.yaml`'s `image:` field to what you pushed in 2B.1, then:

```bash
kubectl apply -f deployment.yaml -f service.yaml
kubectl -n victoria-gateway rollout status deployment/victoria-gateway
```

### 2B.3 Verify: health and a synthetic alert

```bash
kubectl -n victoria-gateway port-forward svc/victoria-gateway 8090:8090 &
curl -i http://localhost:8090/healthz   # expect 200 ok
```

Send the same synthetic alert payload as 2A.6 to
`http://localhost:8090/webhook/alertmanager` through the port-forward,
and watch `kubectl -n victoria-gateway logs deploy/victoria-gateway -f`
the same way 2A.6 watches `docker compose logs -f`.

### 2B.4 Wire it into Alertmanager

In-cluster Alertmanager: point the second `webhook_configs` entry (see
2A.7 for the shape) at
`http://victoria-gateway.victoria-gateway.svc.cluster.local:8090/webhook/alertmanager`.
Alertmanager outside the cluster needs its own path in (Ingress,
LoadBalancer, NodePort) — environment-specific, not picked for you here.

That's the minimal install done (Kubernetes path).

### For the rest of this document

Sections 3 onward show Docker Compose commands (`docker compose up -d
victoria-gateway`, etc.) since that's the path with the longer track
record. On Kubernetes, wherever this doc says "restart," the equivalent
is usually: update the Secret, then

```bash
kubectl -n victoria-gateway rollout restart deployment/victoria-gateway
```

`docker compose exec victoria-gateway <cmd>` becomes `kubectl -n
victoria-gateway exec deploy/victoria-gateway -- <cmd>`. Everything about
`config.yaml`'s *content* — which fields to set, what they do — is
identical either way; only how the file reaches the container differs.

## 3. Optional: enable RAG

Adds similar-past-incident context to the prompt, Pending/Confirmed
capture of every analyzed alert, the `/incidents` and `/pending` pages,
and (optionally) auto-filed Gitea/GitHub issues. Read README's "What data
this stores, and where it goes" before turning this on against a Loki that
holds production logs: with RAG enabled, each alert's log excerpt ends up
in three places, two of which are unauthenticated web pages by default
(section 4 fixes that).

### 3.1 Stand up Postgres + pgvector

If you don't already have one, use the quickstart. It applies
`pkg/rag/schema.sql` automatically on first start, so there's no manual
schema step in this path:

```bash
cd deploy/rag-quickstart
cp .env.example .env
# edit .env, set a real POSTGRES_PASSWORD
docker compose up -d
docker compose logs -f postgres   # wait for "database system is ready to accept connections"
```

The resulting DSN is
`postgres://victoria:<your password>@<this host's LAN IP>:5432/victoria_gateway`.
See `deploy/rag-quickstart/README.md` for the named-volume and
same-docker-network notes; it's a home-lab quickstart, not a managed
Postgres.

If you already have a Postgres, install the pgvector extension at the OS
package level on that server (`CREATE EXTENSION` alone can't install the
binary), then run the schema by hand against the target database:

```bash
psql "postgres://user:pass@host:5432/dbname" -f pkg/rag/schema.sql
```

`schema.sql` declares the embedding column as `vector(1024)`, which is
`bge-m3`'s output dimension. Using a different embedding model means
checking its dimension and editing that column before running the file
(or before the quickstart's first start, since it mounts the same file).

### 3.2 Fill in the `rag:` block

Uncomment the `rag:` block in `config.yaml` and set:

```yaml
rag:
  enabled: true
  postgres_dsn: "postgres://victoria:<password>@<postgres host>:5432/victoria_gateway"
  embedding_endpoint: "http://REPLACE-llm-host:1234"   # OpenAI-compatible /v1/embeddings
  embedding_model: "bge-m3"
  top_k: 3
  public_base_url: "http://<address a browser can reach>:8090"
```

`public_base_url` is what the links in Telegram pushes get prefixed with;
without it the "confirm this one" and similar-incident links aren't
absolute. Leave the `gitea:`/`github:` sub-blocks commented unless you
want an issue filed per alert (README's "RAG" section covers the token
scopes and the dedicated-repo requirement; configure at most one of the
two).

### 3.3 Restart and verify

```bash
docker compose up -d victoria-gateway
docker compose logs -f victoria-gateway
```

Then:

```bash
curl -i http://localhost:8090/incidents
curl -i http://localhost:8090/pending
```

Both should return `200` with an HTML page. Send the synthetic alert from
2A.6 again (new fingerprint) and confirm a row appears on `/pending`, and
that the Telegram push now carries a `/pending/{id}` link.

**Expect `/incidents` and the "相似歷史事件" section to stay empty at
first.** Search only returns Confirmed records and a fresh install has
zero. RAG isn't broken; there's nothing in the store yet to be similar to.
See README's "RAG has nothing to retrieve until something is Confirmed".
To get the first record in, confirm one via the form on `/pending/{id}`,
or from the CLI inside the container:

```bash
docker compose exec victoria-gateway victoria-gateway note --id 1 --resolution "what it actually turned out to be"
```

(`note`, `sync`, and `suppression-candidates` all read the same config
path the server does, `VICTORIA_GATEWAY_CONFIG`, which the image already
sets.) Clean up any smoke-test rows afterwards so they don't get retrieved
as "similar incidents" later.

## 4. Optional: secure it

Two independent Basic Auth gates, both off by default. README's "Securing
the webhook" and "Securing the web UI" explain when each matters; the
short version is that anyone who can reach the port can otherwise trigger
LLM/cloud spend and, once RAG is on, write resolutions via `/pending/{id}`.

Uncomment and fill in `config.yaml`:

```yaml
webhook_auth:
  username: "alertmanager"
  password: "REPLACE-a-real-secret"

webui_auth:
  username: "ops"
  password: "REPLACE-a-real-secret"
```

`webhook_auth` gates `POST /webhook/alertmanager` and `/maintenance-windows`;
the same username/password then goes into Alertmanager's `webhook_configs`
as `http_config.basic_auth` (exact YAML in
`deploy/alertmanager_receiver_example.md`), followed by a reload.
`webui_auth` gates `/incidents`, `/pending`, `/maintenance-windows`, and
`/audit`.

Restart, then confirm the gates are actually up:

```bash
docker compose up -d victoria-gateway
curl -i http://localhost:8090/pending                 # expect 401
curl -i -u ops:REPLACE-a-real-secret http://localhost:8090/pending   # expect 200
```

If you'd rather not have the passwords in the file (CI-templated config,
read-only ConfigMap, secrets from the orchestrator), each credential field
has a `VICTORIA_GATEWAY_*` environment variable that wins over the file
when set. Full table in README's "Environment variable overrides"; for
example:

```yaml
# in the compose service block
environment:
  VICTORIA_GATEWAY_TELEGRAM_BOT_TOKEN: "..."
  VICTORIA_GATEWAY_WEBUI_AUTH_PASSWORD: "..."
  VICTORIA_GATEWAY_RAG_POSTGRES_DSN: "postgres://..."
```

One gotcha: an env var only overrides a field inside a block the file
already configures. `VICTORIA_GATEWAY_WEBUI_AUTH_PASSWORD` does nothing if
there's no `webui_auth:` block at all, so keep the block (with a
placeholder password) in the file.

## 5. Optional: audit log

Records who replaced the maintenance-window set, confirmed a pending
incident, or applied a suppression candidate as a silence. Requires RAG
(section 3), since it reuses that Postgres connection. Reasoning and the
actor semantics are in README's "Audit log".

1. Make sure the `audit_log` table exists. `schema.sql` is idempotent
   (`CREATE TABLE IF NOT EXISTS` throughout), so re-running the whole file
   against an already-provisioned database is safe and just adds the
   missing table. A quickstart database that was initialized before the
   table was added needs this too, because the quickstart only applies
   `schema.sql` to an empty volume:

   ```bash
   # quickstart container
   docker exec -i victoria-gateway-rag-postgres psql -U victoria -d victoria_gateway < pkg/rag/schema.sql

   # or any other Postgres
   psql "postgres://user:pass@host:5432/dbname" -f pkg/rag/schema.sql
   ```

2. Set it in the `rag:` block:

   ```yaml
   rag:
     enabled: true
     audit_log: true
   ```

3. Restart and verify (add `-u ops:...` if `webui_auth` is set):

   ```bash
   docker compose up -d victoria-gateway
   curl -i http://localhost:8090/audit
   ```

   Expect `200`. Confirm a pending record via the web form and it shows up
   as the first row.

## 6. Optional: cloud escalation and the Jev judge

**Cloud escalation.** Uncomment the `cloud:` block in `config.yaml` (and,
if you want deterministic rules, the `escalation:` block with
`always_cloud` and `max_per_hour`), restart, and hard alerts get re-run
against a stronger cloud model while everything else stays local. Gemini
is the default provider; Anthropic, AWS Bedrock, Azure OpenAI, and AWS
DevOps Agent each have their own extra setup covered in README's "Triage:
escalating a hard alert to a cloud model" and its subsections. Two
startup-time facts worth knowing before you edit: an `escalation:` block
without a `cloud:` block is a startup error rather than a silent no-op,
and set `max_per_hour` to something if cloud spend matters to you, since
nothing else bounds it.

**Jev / TypeSafe AI judge.** Uncomment the `judge:` block and set
`judge.api_key`; the feature is entirely disabled while that key is unset.
It's additive only: it can turn a non-escalating alert into an escalating
one, never the reverse, and a failed Jev call falls back to whatever the
existing rules decided. Leave `escalate_threshold` at its 0.70 default
until you've run `cmd/judge-eval` against your own confirmed incidents.
See README's "Jev / TypeSafe AI escalation judge (optional)".

## 7. Optional: `suppression-candidates --apply-silences`

`victoria-gateway suppression-candidates` prints (alertname, host) pairs
that keep getting confirmed the same way, with a permanent-route
suggestion and a time-bounded `amtool silence` alternative. It needs RAG
(it reads confirmed history) and by default only prints. Reasoning in
README's "Turning repeat confirmations into suppression rule candidates".

To have it create the silences for you via Alertmanager's API, uncomment
the `alertmanager:` block in `config.yaml` (`endpoint`, plus
`username`/`password` only if Alertmanager sits behind Basic Auth),
restart, then:

```bash
# dry run: shows what would be created, touches nothing
docker compose exec victoria-gateway victoria-gateway suppression-candidates --min-count 5 --apply-silences

# actually create them
docker compose exec victoria-gateway victoria-gateway suppression-candidates --min-count 5 --apply-silences --yes
```

Without `--yes` it's always a dry run. It only ever creates self-expiring
silences, never the permanent route and never a config-file edit, and it
skips candidates that already have a matching active silence. With
`rag.audit_log` on, each created silence is recorded as `cli:$USER`.

## 8. Optional: external health monitor

victoria-gateway is a single process whose whole job is noticing other
services' silent failures, which makes it a single point of failure for
that noticing: if its host dies, nothing on that host is left to tell you.
`deploy/external-healthcheck/` is a Kubernetes CronJob that curls
`/healthz` every 5 minutes from a *genuinely separate* machine (a
different cluster, not another container on the same host) and pushes a
Telegram message if it can't reach it. It's a curl image and a shell
one-liner, no application code, and it doesn't touch this repo's config.

Follow `deploy/external-healthcheck/README.md` end to end on the other
cluster: apply `namespace.yaml`, create the Telegram Secret imperatively
(the token must not go into `cronjob.yaml`), adjust `BASE_URL` in
`cronjob.yaml` if victoria-gateway isn't at `172.16.100.6:8090`, apply the
CronJob, and run the manual Job it describes once to confirm the check can
actually reach the service. Test the failure path for real once (point
`BASE_URL` somewhere unreachable, re-run, confirm the Telegram message
arrives) before trusting it.

## 9. Post-install verification

How to know it actually works, as opposed to merely running:

- [ ] `curl -i http://localhost:8090/healthz` returns `200 ok`.
- [ ] The synthetic POST from 2A.6 returns `202` (async) or `200` with
      `results` (sync), and the container log shows the Loki query, LLM
      call, and Telegram push with no error lines.
- [ ] The summary for that synthetic alert **arrived in the Telegram chat**.
      Alertmanager never reads the webhook's response body, so this is the
      only place the output reaches a human.
- [ ] A real alert (fire one through Alertmanager, or wait for the next
      one) produced a Telegram summary via the second `webhook_configs`
      entry, and your existing receiver still fired as before.
- [ ] `curl -s http://localhost:8090/metrics` returns Prometheus text
      exposition, and if you added a scrape config for it, the target shows
      as up in Prometheus with `victoria_gateway_alerts_total` (and the
      `victoria_gateway_notify_push_total{channel=...}` counters) moving.
- [ ] *If RAG:* `/pending` shows the test alert's row; `/incidents` is
      empty until you confirm something, which is expected.
- [ ] *If auth:* unauthenticated `curl` to `/pending` and to
      `/webhook/alertmanager` return `401`, and Alertmanager's own
      `basic_auth` delivery still gets through.
- [ ] *If audit log:* `/audit` returns `200` and lists the confirmation you
      just made.
- [ ] *If external healthcheck:* the manual Job printed
      `victoria-gateway healthz OK`.
- [ ] Smoke-test artifacts (pending rows, filed issues) cleaned up.

## 10. Upgrading

```bash
git pull
docker build -t victoria-gateway:latest .
docker compose up -d victoria-gateway
docker compose logs -f victoria-gateway
```

`up -d` recreates the container on the new image. Thanks to
`stop_grace_period`, the old container drains in-flight analyses before
exiting, so an upgrade mid-escalation doesn't lose it.

On Kubernetes: `kubectl apply` only triggers a rollout when the pod
template actually changes — pushing a new image to the same `:latest`
tag and re-applying an unchanged manifest does nothing, since the
Deployment spec looks identical either way. Build and push a new,
distinct tag, edit `deployment.yaml`'s `image:` to it, then `kubectl
apply -f deployment.yaml` and `kubectl -n victoria-gateway rollout
status deployment/victoria-gateway`. (Staying on `:latest` instead and
forcing a fresh pull is possible too — `kubectl -n victoria-gateway
rollout restart deployment/victoria-gateway` — but a distinct tag per
build is easier to reason about and to roll back.)
`terminationGracePeriodSeconds` does the same job `stop_grace_period`
does on Compose.

Your existing `config.yaml` keeps working: every feature added so far has
shipped as an optional block that's off unless configured, and the README
describes each one that way ("omit this whole section to run exactly as
before"). Diff the new `deploy/config.docker.yaml` against your file
after pulling to see what new optional blocks exist, rather than assuming
you need any of them.

The one thing that is not automatic is the database. If you run RAG,
check README's RAG setup notes for the release you're upgrading across:
re-running `pkg/rag/schema.sql` is always safe and picks up new tables
(section 5), while a database from before the Pending/Confirmed split or
before the `embedding_model` column needs
`pkg/rag/migrate_0001_pending_status.sql` and/or
`pkg/rag/migrate_0002_embedding_model.sql` run by hand first, and one from
before batch confirm needs `pkg/rag/migrate_0003_dup_of.sql`.

`migrate_0003` is the only one you can skip without breaking anything:
without the `dup_of` column the pending list still works, it just can't
offer batch confirm and says so on the page (and once at startup). Run it
and restart to turn the feature on. Also don't
change `rag.embedding_model` casually across an upgrade: pgvector only
enforces the dimension, not the model, and the startup warning about
mismatched rows is the only thing that will tell you the old vectors no
longer mean anything (README, "RAG: grounding the summary in past
incidents").
