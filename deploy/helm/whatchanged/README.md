# whatchanged

Answers the first question of every incident: **what changed?**

whatchanged watches your cluster (rollouts, ConfigMaps, Helm releases, Argo CD and Flux syncs, network policies, node churn), accepts changes pushed from CI, Terraform and feature-flag tools, and puts them on one timeline. When something breaks, `whatchanged suspects` ranks the likely culprits and explains why.

It is read-only (`list`/`watch` only), never stores Secret values, and reads Helm releases as metadata only.

## Install

```bash
helm repo add simplyzee https://charts.simplyzee.dev
helm repo update
helm install whatchanged simplyzee/whatchanged -n whatchanged --create-namespace \
  --set persistence.enabled=true
```

Open the UI:

```bash
kubectl -n whatchanged port-forward svc/whatchanged 8080
```

Then browse to http://localhost:8080, or use the CLI (`go install github.com/simplyzee/whatchanged/cmd/whatchanged@latest`):

```bash
whatchanged --since 30m -n payments
whatchanged suspects -n payments
```

## Common settings

| Value | Default | Description |
|---|---|---|
| `persistence.enabled` | `false` | Keep history across restarts (Helm and Warning-event history is backfilled either way) |
| `retention` | `7d` | How long to keep changes |
| `trackers.enable` | `[]` | Extra trackers, e.g. `[secrets]` |
| `trackers.disable` | `[]` | e.g. `[warning-events]` |
| `trackers.excludeNamespaces` | `[kube-node-lease]` | Namespaces to ignore |
| `ingest.enabled` | `true` | Push API for CI, Terraform and flags. A token Secret is generated (`<release>-ingest`) |
| `grafana.url` | `""` | Push changes to Grafana as annotations |
| `grafana.existingSecret` | `""` | Secret with a Grafana service-account token (key `token`) |
| `clusterName` | `""` | Added as a tag on Grafana annotations |
| `serviceMonitor.enabled` | `false` | Prometheus Operator ServiceMonitor |
| `ingress.enabled` | `false` | Expose the UI and API |

See [values.yaml](values.yaml) for everything, and the [project README](https://github.com/simplyzee/whatchanged) for the API, the push examples and how suspects are ranked.
