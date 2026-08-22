# Wiring victoria-gateway into Alertmanager

Add `victoria-gateway`'s webhook as a **second** `webhook_configs` entry on
whatever receiver your routes already send to — additive, doesn't touch
existing notification channels:

```yaml
receivers:
  - name: 'yourExistingReceiver'
    webhook_configs:
      - send_resolved: true
        url: 'http://<your-existing-webhook-target>'
      - send_resolved: true
        url: 'http://victoria-gateway:8090/webhook/alertmanager'
```

That's it — no changes to `route:` needed. Every alert that already reaches
`yourExistingReceiver` now also hits victoria-gateway in parallel; both
integrations fire independently, so a failure in one doesn't block the other.

Reload without a restart:

```bash
curl -X POST http://<alertmanager-host>:9093/-/reload
```

## Notes

- victoria-gateway requires every alert to carry a `host` or `instance` label
  (used to scope the Loki query) — anything without one gets logged as an
  error result rather than crashing the request.
- Watch `repeat_interval` / `group_interval` on your route. A short
  `repeat_interval` (e.g. left over from testing) will re-trigger the LLM
  summary — and the Telegram push — on every repeat, not just the first
  firing. A few hours is a reasonable default for a home lab.
