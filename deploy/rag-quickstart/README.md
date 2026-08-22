# RAG quickstart

Standalone Postgres+pgvector for victoria-gateway's [RAG store](../../README.md#rag-grounding-the-summary-in-past-incidents).
Skips the manual "install the pgvector OS package, run schema.sql by hand"
setup — `pgvector/pgvector:pg16` ships the extension binary already built
in, and `schema.sql` is applied automatically on first start.

## Use

```bash
cp .env.example .env
# edit .env, set a real POSTGRES_PASSWORD

docker compose up -d
docker compose logs -f postgres   # wait for "database system is ready to accept connections"
```

Then in victoria-gateway's `config.yaml`:

```yaml
rag:
  enabled: true
  postgres_dsn: "postgres://victoria:<your password>@<this host's LAN IP>:5432/victoria_gateway"
  embedding_endpoint: "http://your-llm-host:1234"
  embedding_model: "bge-m3"
```

If victoria-gateway runs as a container on the same docker network as this
compose file, you can put both services in one network and use the
`postgres` service name instead of a LAN IP — same idea as
`deploy/docker-compose.snippet.yml`, just not merged by default so this
stays usable standalone.

## Notes

- Data persists in the `pgdata` named volume across restarts. `docker
  compose down` keeps it; `docker compose down -v` deletes it — same as any
  other named-volume Postgres.
- `schema.sql` only runs against an empty data volume (Postgres's own
  `docker-entrypoint-initdb.d` behavior). If you're upgrading an existing
  RAG database that predates this quickstart, run
  `../../pkg/rag/migrate_0001_pending_status.sql` by hand instead — see the
  main README's RAG section.
- This is a dev/home-lab quickstart, not a production Postgres setup — no
  backups, no tuning, a single instance with a named volume. Point
  `postgres_dsn` at a properly managed Postgres instead if you already have
  one.
