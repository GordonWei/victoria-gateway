# External health monitor

victoria-gateway is a single process watching other services for silent
failure — which makes it a single point of failure for that watching. If
the host or container it runs on dies, nothing else notices unless
something outside that same machine is checking. This is that check: a
Kubernetes CronJob that curls `/healthz` from a genuinely different
machine (a separate cluster, not just a separate container on the same
host) every 5 minutes, and pushes a Telegram message if it can't reach it
after 3 quick retries.

It's deliberately zero application code — a curl image and a shell
one-liner, not a new Go binary — and it doesn't touch victoria-gateway's
own repo or config at all. It lives here only because this is where an
operator installing victoria-gateway would look for it.

## Install

1. Point `kubectl` at the cluster this should run from — anywhere that
   isn't the same host/VM as victoria-gateway itself. This was set up
   against a separate home Kubernetes cluster (`kubectl config
   get-contexts` to see what you have); adjust `BASE_URL` in
   `cronjob.yaml` if victoria-gateway isn't at `172.16.100.6:8090`.

2. Create the namespace:

   ```sh
   kubectl apply -f namespace.yaml
   ```

3. Create the Telegram credentials as a Secret — **do not** put the real
   bot token in `cronjob.yaml` or commit it anywhere; this is the one
   manual, imperative step:

   ```sh
   kubectl create secret generic victoria-gateway-healthcheck-telegram \
     -n victoria-gateway-healthcheck \
     --from-literal=bot-token="<your bot token>" \
     --from-literal=chat-id="<your chat id>"
   ```

   Reusing the same bot/chat victoria-gateway itself already pushes to is
   the obvious default — the alert then lands in the same place as every
   other notification — but doesn't have to be the same one.

4. Apply the CronJob:

   ```sh
   kubectl apply -f cronjob.yaml
   ```

5. Verify it actually runs and can reach victoria-gateway, without
   waiting for the next scheduled tick:

   ```sh
   kubectl create job -n victoria-gateway-healthcheck --from=cronjob/victoria-gateway-healthz victoria-gateway-healthz-manual-check
   kubectl logs -n victoria-gateway-healthcheck job/victoria-gateway-healthz-manual-check
   kubectl delete job -n victoria-gateway-healthcheck victoria-gateway-healthz-manual-check
   ```

   A healthy target prints `victoria-gateway healthz OK` and sends no
   Telegram message. If you want to see the failure path fire for real
   before trusting it, temporarily set `BASE_URL` to something
   unreachable (e.g. `http://172.16.100.6:1`), re-run the manual Job, and
   confirm the Telegram message actually arrives — then put `BASE_URL`
   back and re-apply.

## What this intentionally doesn't do

- **No dedup across runs.** A real outage re-sends the Telegram message
  every 5 minutes for as long as it lasts — there's no state store
  tracking "already alerted for this outage." Simpler to reason about,
  and a stream of reminders during a real outage is arguably the
  behavior you want anyway. Raise `schedule` or add a dedup mechanism if
  this turns out to be too noisy in practice.
- **No escalation ladder, no ack, no silence.** This is one check with
  one alert channel — it's the backstop for "is victoria-gateway even
  alive," not a second alerting system.
