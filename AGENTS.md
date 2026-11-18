# AGENTS.md

Instructions for AI agents (Claude Code, Codex, Cursor, Copilot and others) that are asked to **install whatchanged, connect it over MCP, or work on this codebase**.

whatchanged runs in a Kubernetes cluster and records everything that changes: rollouts, ConfigMaps, Helm, Argo CD and Flux, Services and Ingresses, RBAC, admission webhooks, certificates, network policies, nodes, and changes pushed from CI. It ranks the changes most likely to have caused a problem. It is **read-only** in the cluster (`list` and `watch` only).

Pieces:

| Piece | What it is | Where it runs |
|---|---|---|
| **Server** | watcher, store, HTTP API, web UI, MCP endpoint (`POST /mcp`) | in the cluster, installed with Helm |
| **CLI / kubectl plugin** | `whatchanged` / `kubectl whatchanged`, which talks to the server | the user's machine or CI |
| **MCP server (stdio)** | `kubectl whatchanged mcp`, which bridges an AI client to the server | the user's machine |

Always confirm with the user before installing anything into a cluster, and confirm **which kube context** to use. Never print the ingest token into chat or logs unless the user asks.

---

## 1. Install the server (Helm)

Prerequisites: `kubectl` pointed at the target cluster, and `helm` 3.8 or later. Check with:

```bash
kubectl config current-context   # confirm with the user that this is the right cluster
helm version
```

Install:

```bash
helm repo add simplyzee https://charts.simplyzee.dev
helm repo update
helm install whatchanged simplyzee/whatchanged \
  --namespace whatchanged --create-namespace \
  --set persistence.enabled=true \
  --set risk.timezone=UTC          # e.g. Europe/London; used for off-hours flags
```

Keep the release name `whatchanged` and the namespace `whatchanged` unless the user wants otherwise. The kubectl plugin looks for `svc/whatchanged` in namespace `whatchanged` by default. If you change either, pass `--install-namespace` / `--service` to the plugin (or set `WHATCHANGED_NAMESPACE` / `WHATCHANGED_SERVICE`).

Useful values (full list: `helm show values simplyzee/whatchanged`):

| Value | Default | Notes |
|---|---|---|
| `persistence.enabled` | `false` | keep history across restarts (recommended) |
| `retention` | `7d` | |
| `diffs` | `redacted` | `off` / `redacted` / `full`. Secret data is never diffed |
| `risk.businessHours` / `risk.timezone` | `Mon-Fri 08:00-18:00` / `UTC` | changes outside are flagged `off-hours` |
| `risk.freezes` | `[]` | e.g. `["xmas=2026-12-20/2027-01-03"]` |
| `trackers.enable` / `trackers.disable` | `[]` | names from `kubectl whatchanged trackers`. `secrets` is opt-in |
| `ingest.enabled` | `true` | push API and MCP `record_change`. A token Secret `whatchanged-ingest` is generated |
| `grafana.url` + `grafana.existingSecret` | | push changes to Grafana as annotations |
| `clusterName` | | tag on Grafana annotations |

Verify:

```bash
kubectl -n whatchanged rollout status deploy/whatchanged --timeout=2m
kubectl -n whatchanged port-forward svc/whatchanged 8080:8080 &
curl -fsS localhost:8080/readyz        # "ok" once every tracker has synced
curl -fsS localhost:8080/api/v1/status # tracker states: synced / not-installed / forbidden
```

- `not-installed` is normal for optional CRDs the cluster doesn't have (Argo CD, Flux, Cilium, Kong, cert-manager, KEDA, Gateway API).
- `forbidden` means RBAC is missing, which usually means a custom `rbac.create=false` setup.
- The web UI is at http://localhost:8080 while the port-forward runs.

Upgrade with `helm upgrade whatchanged simplyzee/whatchanged -n whatchanged --reuse-values`. Uninstall with `helm uninstall whatchanged -n whatchanged`. The ingest Secret is kept on purpose (`helm.sh/resource-policy: keep`), so delete it by hand if wanted.

## 2. Install the CLI / kubectl plugin

Preferred: krew. This repository is its own krew index.

```bash
kubectl krew index add simplyzee https://github.com/simplyzee/whatchanged.git
kubectl krew install simplyzee/whatchanged
kubectl whatchanged version
```

If krew isn't installed, either install it (https://krew.sigs.k8s.io/docs/user-guide/setup/install/) or use one of these:

```bash
go install github.com/simplyzee/whatchanged/cmd/whatchanged@latest
# or download a binary for the OS/arch from
# https://github.com/simplyzee/whatchanged/releases/latest
```

How the CLI finds the server:

- As `kubectl whatchanged`, it runs `kubectl port-forward` to `svc/whatchanged` in namespace `whatchanged` automatically. Use `--context`, `--kubeconfig`, `--install-namespace` and `--service` to change the target.
- As plain `whatchanged`, it uses `WHATCHANGED_URL` (default `http://localhost:8080`). Add `--port-forward` to get the plugin behaviour.

Smoke test:

```bash
kubectl whatchanged --since 1h
kubectl whatchanged suspects -n <namespace>
kubectl whatchanged -o wide --since 1h     # shows change IDs
kubectl whatchanged show <id>              # one change with its diff
```

## 3. Connect an AI client over MCP

Tools exposed: `suspects`, `what_changed`, `get_change`, `tracker_status`. A fifth, `record_change`, is offered **only** when the client has the ingest token.

### Option A: local stdio via the kubectl plugin (recommended)

Needs the kubectl plugin (section 2) and access to the cluster from the user's machine.

**Claude Code:**

```bash
claude mcp add whatchanged --scope user -- kubectl whatchanged mcp
# a specific cluster:
claude mcp add whatchanged-prod --scope user -- kubectl whatchanged mcp --context prod
```

**Claude Desktop, Cursor, Windsurf and other clients that use an `mcpServers` JSON config:**

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

- Claude Desktop's config file is `~/Library/Application Support/Claude/claude_desktop_config.json` on macOS and `%APPDATA%\Claude\claude_desktop_config.json` on Windows. Merge into the existing `mcpServers` object, don't overwrite the file.
- **Desktop apps often don't inherit the shell `PATH`.** If the server fails to start, use absolute paths: `"command": "/usr/local/bin/kubectl"` (from `which kubectl`). Also make sure `kubectl-whatchanged` is reachable. krew puts it in `~/.krew/bin`, so either add `"env": {"PATH": "/Users/<user>/.krew/bin:/usr/local/bin:/usr/bin:/bin"}` or call the binary directly: `"command": "/Users/<user>/.krew/bin/kubectl-whatchanged", "args": ["mcp", "--context", "prod"]`.
- If the kubeconfig isn't at the default path, add `"--kubeconfig", "/path/to/config"` to `args`.
- To let the agent record its own actions with `record_change`, add `"env": {"WHATCHANGED_TOKEN": "<ingest token>"}`. Fetch the token with the command in section 4. Ask the user before enabling writes.

### Option B: in-cluster HTTP (streamable HTTP, for agents running in or near the cluster)

The server answers MCP at `POST /mcp`, e.g. `http://whatchanged.whatchanged.svc:8080/mcp`.

- Read-only tools need no auth.
- `record_change` is only listed when the request carries `Authorization: Bearer <ingest token>`.
- Browser `Origin` headers from other hosts are refused.

```bash
claude mcp add --transport http whatchanged http://whatchanged.whatchanged.svc:8080/mcp
# with write access:
claude mcp add --transport http whatchanged http://whatchanged.whatchanged.svc:8080/mcp \
  --header "Authorization: Bearer $WHATCHANGED_TOKEN"
```

Don't expose `/mcp` or the API outside the cluster without an authenticating ingress. Change data reveals a lot about the cluster.

### Verify the MCP connection

```bash
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  | kubectl whatchanged mcp
```

You should see two JSON responses, the second listing `suspects`, `what_changed`, `get_change` and `tracker_status`. Then ask the client something like "what changed in the payments namespace in the last hour?".

### How agents should use the tools during an incident

1. `suspects` with the affected `namespace` (and `at` if the problem started earlier). Read the `why` reasons, not just the scores.
2. `get_change` on the top one or two IDs to see the diff.
3. `what_changed` with filters (`since`, `kind`, `source`, `flag=manual`) for wider context.
4. After taking an action (restart, rollback, flag flip), call `record_change` if available, so humans see it on the timeline.

Present suspects as *likely* causes with their reasons. The ranking is a heuristic, not proof.

## 4. Push changes from CI and other tools

The ingest token lives in the Secret the chart creates:

```bash
kubectl -n whatchanged get secret whatchanged-ingest -o jsonpath='{.data.token}' | base64 -d
```

Store it as a CI secret (`WHATCHANGED_TOKEN`) along with the server URL (`WHATCHANGED_URL`). The CI runner must be able to reach the service, via an internal ingress or similar. Then:

```bash
whatchanged push --source github-actions --name <service> -n <namespace> \
  --summary "deploy <version>" --impact high --url "<run url>"
```

Ready-made examples are in [`examples/`](examples/): a GitHub Actions step and a Terraform wrapper. Pushes are idempotent.

## 5. Troubleshooting

| Symptom | Fix |
|---|---|
| `port-forward to svc/whatchanged in namespace whatchanged failed` | server not installed, a different release name or namespace (use `--service` / `--install-namespace`), or the wrong context (`--context`) |
| `cannot reach whatchanged at http://localhost:8080` | plain `whatchanged` without a port-forward: add `--port-forward` or set `WHATCHANGED_URL` |
| `/readyz` stays 503 | a tracker is still listing (big clusters take a little while). Check `/api/v1/status` and the pod logs |
| tracker shows `forbidden` | the ClusterRole is missing a resource. Use the chart's RBAC or add the rule |
| MCP client shows no tools / server exits | PATH or kubeconfig not visible to the app. Use absolute paths and `--kubeconfig` (section 3) |
| `record_change` missing | no token: set `WHATCHANGED_TOKEN` (stdio) or the `Authorization` header (HTTP) |
| everything flagged `off-hours` | set `risk.timezone` / `risk.businessHours`, or `risk.businessHours=off` |

Logs: `kubectl -n whatchanged logs deploy/whatchanged`.

---

## 6. Working on this codebase

**Rules**

- **Zero dependencies.** `go.mod` has no `require` block, and it stays that way. Use the Go standard library only, with no client-go and no YAML library. The Kubernetes client is in `internal/kube`.
- **Read-only.** Trackers only `list` and `watch`. Never add verbs that write to the cluster.
- **Never store or log Secret values.** Hash them. Diffs pass through redaction in `internal/tracker/flatten.go`.
- Run `make lint test` before committing (`gofmt`, `go vet`, `go test -race ./...`).

**Layout**

| Path | |
|---|---|
| `cmd/whatchanged` | entry point. Plugin mode is detected from `argv[0]` (`kubectl-whatchanged`) |
| `internal/kube` | list/watch client and reflector. `kubetest` is a fake API server for tests |
| `internal/tracker` | trackers. `fields.go` is the generic field-fingerprint engine, `sources.go` has most resource types, `graph.go` the owner/config graph, `runner.go` the registry |
| `internal/store` | in-memory store with JSONL persistence |
| `internal/suspects` | ranking |
| `internal/risk` | manual / off-hours / freeze flags |
| `internal/api` | HTTP API and embedded UI (`ui/index.html`) |
| `internal/mcp` | MCP server (stdio and HTTP) |
| `internal/cli` | commands. `connect.go` handles port-forward |
| `internal/e2e` | end-to-end test against the fake API server |
| `deploy/helm/whatchanged` | Helm chart |
| `hack/krewmanifest` | generates `plugins/whatchanged.yaml` on release |

**Adding a tracker for a new resource type**

1. Add a `fieldTracker(FieldSpec{...})` in `internal/tracker/sources.go`, or a custom `Spec[T]` if it needs special diffing. Set `Optional: true` for CRDs, and give API version alternatives in `Paths`.
2. Register it in `Registry` in `internal/tracker/runner.go`.
3. Add `list` and `watch` for the resource to `deploy/helm/whatchanged/templates/rbac.yaml`.
4. Add a test in `internal/tracker/sources_test.go`, and a row in the README "What it tracks" table.

**Releasing (maintainers)**

1. Bump `version`, `appVersion` and the `artifacthub.io/images` tag in `deploy/helm/whatchanged/Chart.yaml`, and update `artifacthub.io/changes`.
2. Merge to `main` and wait for CI (test, chart, krew, kind).
3. Run the `release` workflow, either by pushing a `vX.Y.Z` tag or by dispatching it with `version=vX.Y.Z`. It publishes the multi-arch image to `ghcr.io/simplyzee/whatchanged`, creates the GitHub release (binaries, chart `.tgz`, checksums, krew manifest) and commits `plugins/whatchanged.yaml` to `main`.
4. Publish the chart to `simplyzee/simplyzee-charts`:
   - Create a `publish-whatchanged-X.Y.Z` branch off `gh-pages`.
   - Add the release's chart `.tgz` and a new `index.yaml` entry (newest first, `digest` = sha256 of the `.tgz`, `urls` = `https://charts.simplyzee.dev/whatchanged-X.Y.Z.tgz`).
   - Open a PR into `gh-pages` and merge it.

Commits use the author email `zee@simplyzee.dev`.
