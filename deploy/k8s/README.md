# Kubernetes deployment

An alternative to the docker-compose path (`deploy/docker-compose.snippet.yml`,
`deploy/config.docker.yaml`) for running victoria-gateway itself on a
Kubernetes cluster instead of a single Docker host. Same binary, same
`config.yaml` shape — this is a direct translation of the compose service
into a Deployment + Service + Secret, not a redesign. Pick whichever
matches where you already run things; nothing else in this repo assumes
one over the other.

## Install

1. **Build and push the image.** Your cluster's nodes need to be able to
   pull it, so a locally-built image alone isn't enough unless every
   node can load it directly (single-node clusters only). Build for the
   architecture your nodes actually run (check with `kubectl get nodes
   -o wide`, look at the `CONTAINER-RUNTIME`/architecture columns — most
   clusters are `linux/amd64`, but don't assume):

   ```sh
   docker build --platform linux/amd64 -t <your-registry>/victoria-gateway:latest .
   docker push <your-registry>/victoria-gateway:latest
   ```

   `<your-registry>` is wherever your cluster already pulls images
   from — Docker Hub, GHCR, a private registry, or a self-hosted one
   (Gitea's own built-in container registry works too, if your Gitea
   version has packages enabled). If it's a private/self-signed
   registry, see "Self-signed or private registries" below before you
   apply anything — a `docker push` succeeding from your own machine
   does **not** mean every node in the cluster can pull it back down;
   those are two separate trust stores.

2. **Create the namespace:**

   ```sh
   kubectl apply -f namespace.yaml
   ```

3. **Create the config Secret** from a real `config.yaml` (see the main
   README's "Config" section, or copy `deploy/config.docker.yaml` and
   fill it in) — same file the docker-compose path bind-mounts, just
   delivered as a Secret instead of a host file:

   ```sh
   kubectl create secret generic victoria-gateway-config \
     -n victoria-gateway \
     --from-file=config.yaml=./config.yaml
   ```

   The key must be exactly `config.yaml` — `deployment.yaml` mounts the
   whole Secret as a directory and the container reads
   `/etc/victoria-gateway/config.yaml`.

4. **Set the image** in `deployment.yaml` (replace the
   `REPLACE-your-registry/...` placeholder with what you pushed in step
   1), then apply:

   ```sh
   kubectl apply -f deployment.yaml -f service.yaml
   ```

5. **Verify it's actually running**, not just scheduled:

   ```sh
   kubectl -n victoria-gateway rollout status deployment/victoria-gateway
   kubectl -n victoria-gateway port-forward svc/victoria-gateway 8090:8090 &
   curl -i http://localhost:8090/healthz   # expect 200 ok
   ```

   Then send a real synthetic alert through the same port-forward and
   confirm it actually processes (Loki query, LLM call) — see the main
   README's "Verify: a synthetic alert" for the exact payload shape and
   `../../DEPLOYMENT.md` for the full walkthrough. `kubectl -n
   victoria-gateway logs deploy/victoria-gateway -f` while you send it.

6. **Point Alertmanager at it.** In-cluster:
   `http://victoria-gateway.victoria-gateway.svc.cluster.local:8090/webhook/alertmanager`.
   Outside the cluster, you need your own path in (Ingress, LoadBalancer,
   NodePort) — see `service.yaml`'s comment.

## Self-signed or private registries

`docker push` succeeding only proves *your machine* trusts the
registry's certificate. Every node's container runtime (containerd for
most clusters, including k3s/RKE2) has its own, separate trust store,
and a node that doesn't trust the registry's cert will fail to pull with
something like `x509: certificate signed by unknown authority`, even
though the image is sitting there and your own `docker login`/`push`
worked fine. Test a real pull before assuming this is done — a throwaway
pod is the fastest way:

```sh
kubectl run pull-test --image=<your-registry>/victoria-gateway:latest --restart=Never -- /usr/local/bin/victoria-gateway --help
kubectl get pod pull-test   # watch for ImagePullBackOff
kubectl delete pod pull-test --now
```

Fixing a failed pull is runtime-specific and node-local — for k3s/RKE2 it
means a `registries.yaml` (see your distribution's docs for the exact
path and format) pointing at the registry's CA certificate, applied on
**every node** the workload might be scheduled to, followed by a restart
of the node's container-runtime service. This repo doesn't ship that
file because the right CA/trust setup is specific to your registry and
your cluster, not something a generic manifest here could get right for
everyone.

## What this intentionally doesn't do

- **No Helm chart.** Three flat manifests are enough for what this is —
  a single-replica Deployment, a Service, and a Secret. A chart would
  add templating machinery for values (registry, resource limits,
  replica count that must stay 1) that don't vary enough to justify it
  yet. If that changes, revisit.
- **No HorizontalPodAutoscaler, no multi-replica anything.** See
  `deployment.yaml`'s `replicas: 1` comment — this isn't a missing
  feature, it's a correctness requirement until dedup/maintenance-window
  state moves into Postgres.
- **No Ingress.** Exposing this outside the cluster is environment-
  specific (TLS termination, hostname, ingress controller in use) and
  left to you, same as the docker-compose path leaves external exposure
  to you.
- **No PodDisruptionBudget.** With `replicas: 1` there's nothing a PDB
  would meaningfully protect — a single pod is always disruptable by
  definition.
