# Deploying Mainspring

Mainspring ships as one static Go binary and a container image, so the same
server runs on a laptop, a shared GPU box, or a GPU node in Kubernetes.

## Container

```bash
# Build
docker build -t mainspring:dev .

# Run (adopting a host Ollama daemon; no local engine baked into the image)
docker run --rm -p 11500:11500 \
  -v "$PWD/deploy/example-config:/config:ro" \
  mainspring:dev

curl localhost:11500/healthz
curl localhost:11500/capabilities
```

The image is distroless (non-root, no shell) and contains **no inference engine**.
Provide one by:
- pointing `backend: ollama` at a reachable daemon (`ollama_host`), or
- baking an engine into a derived image, or
- running `mainspring install <backend>` onto a mounted `/home/nonroot/.config/mainspring` volume.

## Kubernetes (Helm)

```bash
helm install ms deploy/helm/mainspring \
  --set image.tag=latest \
  --set-file config=my-config.yaml   # or edit values.yaml `config:`

# GPU node:
helm install ms deploy/helm/mainspring \
  --set 'resources.limits.nvidia\.com/gpu=1' \
  --set 'nodeSelector.gpu=true'
```

The full `config.yaml` is rendered into a Secret (it may hold tenant keys) and
mounted at `/config/config.yaml`. Either shape works: a map under `config:` in a
values file, or a whole config file via `--set-file config=…`. Model weights
mount at `/models` (`models.persistence.enabled=true` for a PVC). The liveness
probe hits `/healthz` and the readiness probe `/readyz`, so a draining or
all-backends-down pod stops receiving traffic without being restarted; scrape
`/metrics` for Prometheus.

### GPU, autoscaling, and monitoring

```bash
# Ready-made GPU overrides (nvidia.com/gpu, nodeSelector, tolerations, PVC)
helm install ms deploy/helm/mainspring -f deploy/helm/mainspring/values-gpu.yaml

# Prometheus Operator scraping (monitoring.coreos.com CRD required)
helm install ms deploy/helm/mainspring --set serviceMonitor.enabled=true

# CPU-backed autoscaling (HPA; leave off for 1:1 GPU pods)
helm install ms deploy/helm/mainspring \
  --set autoscaling.enabled=true --set autoscaling.maxReplicas=6
```

When `autoscaling.enabled=true` the Deployment omits `replicas` (the HPA owns
it). `values-gpu.yaml` also bounds concurrency (`max_inflight`/`max_queue`) so a
single GPU degrades gracefully under load.

### Validating the chart

```bash
./deploy/helm/validate.sh
```

CI runs this on every PR. It renders every combination an operator can install —
including HPA and ServiceMonitor, which both shipped values files leave off —
schema-validates each with `kubeconform -strict`, and checks that `Chart.yaml`
still tracks the released version, since `image.tag` defaults to `appVersion` and
a stale value means `helm install` quietly pulls an old image.

The check that `helm lint` cannot do is the last one: it extracts the `config.yaml`
the chart writes into its Secret and feeds it to `mainspring doctor`. The chart can
emit perfectly valid Kubernetes containing a config the server refuses to parse —
that is precisely the shape of #207, where a chart-level bug produced a crash-loop
that rendered and schema-validated cleanly.
