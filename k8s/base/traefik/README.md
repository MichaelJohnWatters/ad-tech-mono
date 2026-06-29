# Traefik — Ingress controller for the platform

Colima starts k3s with `--disable=traefik` (see `~/.colima/default/colima.yaml`),
so we install our own. `install.yaml` is a snapshot of the Helm chart
output with these values:

```yaml
ports.web.exposedPort: 80
service.type: NodePort
ports.web.nodePort: 30080
ingressClass.enabled: true
ingressClass.isDefaultClass: true
```

Regenerate with:

```bash
helm template traefik traefik/traefik --namespace traefik \
    --set ports.web.exposedPort=80 \
    --set service.type=NodePort \
    --set ports.web.nodePort=30080 \
    --set ingressClass.enabled=true \
    --set ingressClass.isDefaultClass=true > install.yaml
```

(Requires `helm repo add traefik https://traefik.github.io/charts`.)

The `traefik` namespace is created automatically by k8s when the
manifest is applied.

## Reachability

Traefik's LoadBalancer service gets Colima's k3s LB IP — typically
`192.168.5.1`. /etc/hosts entries for `*.adtech.local` should point at
that IP (handled by `scripts/setup-hosts.sh`, which auto-detects the
current value via `kubectl`).

## Ingress definitions

Each podified service ships its own `ingress.yaml` alongside its
Deployment + Service (see `k8s/base/{dsp,ssp,exchange,…}/`). Traefik
picks them up automatically because it's the default IngressClass.
