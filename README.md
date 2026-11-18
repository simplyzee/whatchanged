# whatchanged

**The first question in every incident is "what changed?". `whatchanged` answers it in one command.**

It runs in your cluster, watches everything that tends to break things (rollouts, ConfigMaps, Helm releases, GitOps syncs, ingress and service routing, RBAC, admission webhooks, network policies, certificates, node churn), accepts changes pushed from CI, Terraform and feature-flag tools, and puts it all on **one timeline**. When something breaks, it **ranks the likely culprits** and tells you why. It also links each failing pod back to the change that broke it.

Use it from the terminal (`kubectl whatchanged`), the web UI, Grafana, or your AI agent over **MCP**.

```console
$ kubectl whatchanged suspects -n payments
Most likely culprits for a problem at 01:13 Wed 18 Nov (1h0m0s window, 3 warning event(s)):

SCORE  AGE  IMPACT  SOURCE      NAMESPACE  OBJECT                CHANGE
77     2s   medium  kubernetes  payments   ConfigMap/api-config  data keys changed POOL_SIZE [manual]
71     2s   high    kubernetes  payments   Deployment/web        web: ghcr.io/acme/web:2.0.0 → 2.1.0

#1 ConfigMap/api-config: medium impact config, 2s before, same namespace, pods of Deployment/api (which use it) then failed: 3 warning(s): BackOff, manual change (kubectl-edit)
#2 Deployment/web: high impact image, 2s before, same namespace

See the diff: kubectl whatchanged show 9e91cfe4
```

The image bump on `web` is the bigger change, but `api`'s pods are the ones crash-looping, and `api` mounts the ConfigMap someone just hand-edited. That's the one to look at first.

![whatchanged timeline](docs/ui-timeline.png)

- **Zero dependencies.** Pure Go standard library: no client-go, no database. One ~7 MB static binary.
- **Read-only.** It only ever `list`s and `watch`es. Secret values are never stored, and Helm releases are read as metadata only.
- **Explainable.** Every suspect score comes with its reasons. No ML black box.
- **Small footprint.** 20m CPU and 64Mi memory requested.

## What it tracks

| Tracker | What you see | Default |
|---|---|---|
| `deployments`, `statefulsets`, `daemonsets` | image changes (`api: repo:1.2 → 1.3`), *which* template fields changed (`containers[api].env`, `pod.tolerations`…), rollout restarts, scaling, pause/resume, create/delete | on |
| `configmaps` | which keys changed, were added or removed | on |
| `helm-releases` | installs, upgrades, rollbacks and failures, backfilled on start from Helm's release history | on |
| `services` | selector, port and type changes (high impact), load balancer annotations | on |
| `ingresses` | per-host routing changes, TLS, class and annotations (e.g. `konghq.com/plugins`) | on |
| `gateway-api` | Gateways (per listener), HTTPRoutes and GRPCRoutes (per rule) | on if the CRDs exist |
| `hpas` | `minReplicas 2 → 3`, metric changes, plus a signal when an HPA hits `maxReplicas` | on |
| `keda-scaledobjects` | trigger and replica bound changes | on if the CRD exists |
| `pdbs`, `priorityclasses`, `resourcequotas`, `limitranges` | the "why won't it schedule / why was it evicted" changes (`hard limits.cpu 10 → 4`) | on |
| `crds` | versions served or stored, schema and conversion changes. Dropping a served version is high impact | on |
| `admission` | validating and mutating webhooks (`failurePolicy Ignore → Fail` gets called out), ValidatingAdmissionPolicies. `caBundle` rotation is ignored | on |
| `rbac` | Roles and ClusterRoles (`added rule get secrets`), bindings (`granted to User:bob (ClusterRole/cluster-admin)`). Wildcards, secrets and cluster-admin are high impact | on |
| `argocd-applications` | syncs (`synced 3f9a1c2e → 8d0b77aa`), apps going Degraded | on if the CRD exists |
| `flux-kustomizations`, `flux-helmreleases` | applied revision changes, Ready=False | on if the CRDs exist |
| `networkpolicies` | policy created / rules changed / deleted | on |
| `cilium-networkpolicies`, `cilium-clusterwidenetworkpolicies` | same, for Cilium (CNP and CCNP) | on if the CRDs exist |
| `cilium-networking` | egress gateway policies, BGP (peering policies, cluster/peer configs, advertisements), LB IPAM pools, L2 announcements | on if the CRDs exist |
| `cert-manager` | Certificate spec changes and renewals, Issuer/ClusterIssuer changes, plus signals when they go not-ready | on if the CRDs exist |
| `kong` | KongPlugin / KongClusterPlugin config (`rate-limiting plugin: config.minute 100 → 10`), KongIngress | on if the CRDs exist |
| `nodes` | joins, removals, cordons, NotReady, kubelet upgrades | on |
| `warning-events` | `BackOff`, `OOMKilling`, `FailedScheduling`… recorded as *signals*, attributed to their owning workload. They're evidence for suspects, never suspects themselves | on |
| `owner-index` | Pod, ReplicaSet and Job metadata, used to link symptoms to changes. Records nothing itself | on |
| `secrets` | which keys changed (values are hashed in memory, never stored or diffed) | **opt-in** |
| **push API** | anything else: CI deploys, Terraform applies, feature flags, DB migrations, vendor incidents, agent actions | when a token is set |

Each change records an **actor**: the Kubernetes field manager that last touched the object (`kubectl-edit`, `argocd-controller`, `helm`, `kustomize-controller`…). That's often enough to tell a human hotfix from a GitOps rollout.

## Smarter answers

- **Symptoms are linked to causes.** whatchanged follows owner references (Pod → ReplicaSet → Deployment, Pod → Job → CronJob) and records which ConfigMaps and Secrets each workload uses. When `api`'s pods start crash-looping, the ConfigMap that `api` mounts outranks a bigger change to an unrelated workload: `pods of Deployment/api (which use it) then failed: 3 warning(s): BackOff`.
- **Diffs, redacted by default.** Every update carries a line diff of the object. In the default `redacted` mode, env var values, ConfigMap data, `caBundle`s and anything whose key looks like a password, token or key are shown as a short hash, so you see *that* they changed but not the value. `--diffs=full` shows values and `--diffs=off` disables diffs. Secret data is never diffed. See a diff with `whatchanged show <id>` or by clicking a change in the UI.
- **Risk flags.** Changes are flagged `manual` (made with kubectl, k9s or Lens), `off-hours` (outside `--business-hours`) or `freeze` (inside a `--freeze` window). Flags show in every view, can be filtered (`--flag manual`), and nudge suspect scores up with the reason spelled out.


## Install

```bash
helm repo add simplyzee https://charts.simplyzee.dev
helm repo update
helm install whatchanged simplyzee/whatchanged \
  -n whatchanged --create-namespace \
  --set persistence.enabled=true
```

Then open the UI:

```bash
kubectl -n whatchanged port-forward svc/whatchanged 8080
open http://localhost:8080
```

### CLI and kubectl plugin

Install the kubectl plugin with [krew](https://krew.sigs.k8s.io/). This repository is its own krew index:

```bash
kubectl krew index add simplyzee https://github.com/simplyzee/whatchanged.git
kubectl krew install simplyzee/whatchanged
```

As a plugin it finds the server by itself, running `kubectl port-forward` to `svc/whatchanged` in the `whatchanged` namespace behind the scenes. `--context`, `--kubeconfig`, `--install-namespace` and `--service` point it elsewhere.

You can also install the standalone binary with `go install github.com/simplyzee/whatchanged/cmd/whatchanged@latest` or from the releases page. It talks to `WHATCHANGED_URL` (default `http://localhost:8080`), or add `--port-forward` to get the plugin behaviour.

```bash
kubectl whatchanged                          # last hour, everything
kubectl whatchanged --since 30m -n payments  # one namespace
kubectl whatchanged --impact high -o wide    # only the scary stuff, with IDs, actor and timestamp
kubectl whatchanged --flag manual --since 7d # who's been hand-editing prod?
kubectl whatchanged --source helm,argocd     # just releases and syncs
kubectl whatchanged suspects -n payments     # what probably broke payments just now?
kubectl whatchanged suspects --at 2026-11-17T14:05:00Z --window 2h   # …or at 14:05
kubectl whatchanged show 3f9a1c2e            # one change with its diff
kubectl whatchanged -o json | jq             # scriptable
```

## AI agents (MCP)

whatchanged is an [MCP](https://modelcontextprotocol.io) server, so Claude and other agents can ask what changed during an incident. Tools:

| Tool | |
|---|---|
| `suspects` | rank likely culprits for a problem (namespace, time, window) |
| `what_changed` | list changes with every filter the CLI has |
| `get_change` | one change with its redacted diff |
| `tracker_status` | what's being watched in this cluster |
| `record_change` | let the agent record its own actions (restarts, flag flips, rollbacks) on the timeline. Only offered with the ingest token |

**Local (stdio)**, e.g. Claude Desktop or Claude Code, through the kubectl plugin's port-forward:

```json
{
  "mcpServers": {
    "whatchanged": {
      "command": "kubectl",
      "args": ["whatchanged", "mcp", "--context", "prod"]
    }
  }
}
```

**In-cluster (streamable HTTP)**: the server answers MCP at `POST /mcp` (e.g. `http://whatchanged.whatchanged:8080/mcp`). Read-only tools need no auth. `record_change` is only listed for clients that send `Authorization: Bearer <ingest token>`.

## Pushing changes from outside the cluster

Half of all "what changed" answers live outside Kubernetes. Push them in:

```bash
export WHATCHANGED_URL=https://whatchanged.internal WHATCHANGED_TOKEN=...

whatchanged push --source terraform --name prod-vpc --action config --impact high \
  --summary "apply: 2 changed, 1 destroyed"

whatchanged push --source launchdarkly --kind FeatureFlag --name new-pricing \
  -n payments --action flag --summary "enabled for 50%"
```

Or with plain HTTP:

```bash
curl -X POST "$WHATCHANGED_URL/api/v1/changes" \
  -H "Authorization: Bearer $WHATCHANGED_TOKEN" \
  -d '{"source":"github-actions","name":"checkout","namespace":"payments",
       "summary":"deploy v2.3.1","impact":"high","actor":"zee",
       "url":"https://github.com/acme/checkout/actions/runs/123"}'
```

The chart generates the token for you:

```bash
kubectl -n whatchanged get secret whatchanged-ingest -o jsonpath='{.data.token}' | base64 -d
```

See [`examples/`](examples/) for a GitHub Actions step and a Terraform wrapper. Pushes are idempotent: the same change sent twice is stored once.

## Grafana

**Annotations on every dashboard (push):** give whatchanged a Grafana service-account token and every medium- or high-impact change shows up as an annotation on your existing panels.

```bash
kubectl -n whatchanged create secret generic grafana-token --from-literal=token=glsa_...
helm upgrade whatchanged simplyzee/whatchanged -n whatchanged --reuse-values \
  --set grafana.url=http://grafana.monitoring.svc \
  --set grafana.existingSecret=grafana-token \
  --set clusterName=prod-eu-west-1
```

Annotations are tagged `whatchanged`, the source, the action, the impact, `ns:<namespace>` and `cluster:<name>`, so you can filter them per dashboard.

**Query on demand (pull):** with the Infinity datasource, add an annotation query against
`http://whatchanged.whatchanged:8080/api/v1/grafana/annotations?from=${__from}&to=${__to}&namespace=payments`.

## How suspects are ranked

For a problem starting at time **T**, every change in the window before T gets a score from 0 to 100:

1. **Impact.** Image changes, Helm releases, GitOps syncs, network policy changes and NotReady nodes are *high*. Config and template changes, restarts and cordons are *medium*. Scaling and new ConfigMaps are *low*.
2. **Recency.** The score halves every quarter of the window. Older changes keep a small floor because slow burns happen.
3. **Locality.** If you pass `-n`, changes in that namespace count fully, cluster-wide changes (nodes, CCNPs, Terraform) count 80%, and other namespaces count 35%.
4. **Evidence.** Warning events within 15 minutes *after* a change boost it. Symptoms on the workloads the change affects (the workload itself, or the workloads using a changed ConfigMap or Secret) count strongly: `its pods then failed: 3 warning(s): BackOff`. Other symptoms in the namespace count weakly, and symptoms on a *different* known workload don't count.
5. **Risk flags.** Changes made during a freeze, by hand (kubectl) or outside business hours get a small boost, with the reason shown.

It's a heuristic, not an oracle. It's built to give you the right first three things to look at.

## API

| Endpoint | |
|---|---|
| `GET /api/v1/changes` | `since`, `until` (`30m`, `2h`, `1d`, RFC 3339 or unix ms), `namespace`, `kind`, `source`, `action`, `flag` (comma-separated), `impact` (minimum), `q`, `signals=false`, `diff=true`, `limit` |
| `GET /api/v1/changes/{id}` | one change with its diff. Accepts an ID prefix of 6 or more characters |
| `GET /api/v1/suspects` | `at`, `window`, `namespace`, `limit` |
| `POST /api/v1/changes` | Bearer token. One change or an array of up to 100. Fields: `source` (required), `name`, `summary`, `namespace`, `kind`, `action`, `impact`, `actor`, `url`, `labels`, `time` |
| `GET /api/v1/grafana/annotations` | `from`, `to` (ms), plus the `changes` filters |
| `GET /api/v1/status` | tracker states (`synced`, `not-installed`, `forbidden`), store size |
| `GET /metrics` | Prometheus metrics: `whatchanged_changes_total{source,action}`, `whatchanged_changes_stored`, `whatchanged_trackers_synced`, … |
| `POST /mcp` | MCP over streamable HTTP |
| `GET /healthz`, `GET /readyz` | ready once every tracker has finished its first list |

## Configuration

`whatchanged server -h` lists every flag. The important ones:

| Flag | Default | |
|---|---|---|
| `--data-dir` | *(memory)* | persist to `changes.jsonl` (append-only, compacted on retention) |
| `--retention` | `7d` | |
| `--enable` / `--disable` | | tracker names, comma separated (`whatchanged trackers` lists them) |
| `--exclude-namespaces` | `kube-node-lease` | |
| `--ingest-token-file` | | or `WHATCHANGED_INGEST_TOKEN`. The push API is disabled without a token |
| `--grafana-url`, `--grafana-token-file`, `--grafana-min-impact` | | |
| `--diffs` | `redacted` | `off`, `redacted` or `full` |
| `--business-hours`, `--timezone` | `Mon-Fri 08:00-18:00`, `UTC` | changes outside are flagged `off-hours` (`off` to disable) |
| `--freeze` | | `[name=]start/end`, e.g. `xmas=2026-12-20/2027-01-03` (repeatable). Changes inside are flagged `freeze` |
| `--api-server` | in-cluster | for local dev: `kubectl proxy` then `--api-server http://127.0.0.1:8001` |
| `--no-kubernetes` | | push API only, no cluster watching |

## Running locally

```bash
make dev   # starts kubectl proxy and runs the server against your current context
```

Without a cluster:

```bash
WHATCHANGED_INGEST_TOKEN=dev go run ./cmd/whatchanged server --no-kubernetes --log-format text
```

## Design notes

- **No client-go.** A ~450-line list/watch client handles pagination, bookmarks, `410 Gone` relists and backoff. The dependency graph is empty, so builds are fast and supply-chain review is trivial.
- **Diff, don't log.** Each tracker keeps a tiny fingerprint per object (image map, per-field hashes, per-key hashes) and diffs it. That's how it can say *which* env block or ConfigMap key changed without storing any contents. Most trackers share one generic field-fingerprint engine, so adding a new resource type is a few lines.
- **Owner graph from metadata only.** Pods, ReplicaSets and Jobs are watched as `PartialObjectMetadata`, so linking symptoms to workloads costs little memory and never reads a pod spec.
- **Relists are diffs too.** If a watch expires, the relist is compared against known state, so changes during the gap aren't lost.
- **Deterministic IDs.** Every change has a content-derived ID, so restarts, relists and Helm or Argo backfills never create duplicates.
- **Single replica by design.** The store is a single-writer append-only log. Keep `replicaCount: 1`, because changes are cheap to re-derive.

## Roadmap

- Native webhooks for GitHub deployments, Argo CD notifications and Terraform Cloud run notifications
- AWS CloudTrail (via EventBridge), LaunchDarkly/Unleash, Vault audit and Grafana/Alertmanager rule changes
- Alertmanager receiver: attach top suspects to firing alerts
- Multi-cluster: point several clusters at one central store
- Submission to the central krew-index

## Contributing

Issues and PRs welcome. `make lint test` must pass. The end-to-end tests run against a fake API server (`internal/kube/kubetest`), and CI also installs the chart into a kind cluster and checks real changes are recorded.

## License

Apache 2.0
