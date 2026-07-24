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
mounted at `/config/config.yaml`. Model weights mount at `/models`
(`models.persistence.enabled=true` for a PVC). Liveness/readiness probes hit
`/healthz`; scrape `/metrics` for Prometheus.
