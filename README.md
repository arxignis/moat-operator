<p align="center">
  <img src="./images/logo.svg" alt="Gen0Sec" width="280">
</p>

<p align="center">
  <a href="https://github.com/gen0sec/synapse-operator/blob/main/LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-green" alt="License - Apache 2.0"></a> &nbsp;
  <a href="https://github.com/gen0sec/synapse-operator/releases"><img src="https://img.shields.io/github/release/gen0sec/synapse-operator.svg?label=Release" alt="Release"></a> &nbsp;
  <img alt="GitHub Downloads (all assets, all releases)" src="https://img.shields.io/github/downloads/gen0sec/synapse-operator/total"> &nbsp;
  <a href="https://docs.gen0sec.com/"><img alt="Documentation" src="https://img.shields.io/badge/gen0sec-documentation-page?style=flat&link=https%3A%2F%2Fdocs.gen0sec.com%2F"></a> &nbsp;
  <a href="https://discord.gg/jzsW5Q6s9q"><img src="https://img.shields.io/discord/1377189913849757726?label=Discord" alt="Discord"></a> &nbsp;
  <a href="https://x.com/gen0sec"><img src="https://img.shields.io/twitter/follow/gen0sec?style=flat" alt="X (formerly Twitter) Follow" /></a>
</p>

<p align="center">
  <a href="https://discord.gg/jzsW5Q6s9q"><img src="https://img.shields.io/badge/Join%20Us%20on-Discord-5865F2?logo=discord&logoColor=white" alt="Join us on Discord"></a>
  <a href="https://gen0sec.substack.com/"><img src="https://img.shields.io/badge/Substack-FF6719?logo=substack&logoColor=fff" alt="Substack"></a>
</p>

---

## Keep Synapse in Sync on Kubernetes

A Go [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime) operator for running [Synapse](https://github.com/gen0sec/synapse) on Kubernetes. It runs in **two modes** from the same binary: a **config-sync controller** that rolls Synapse pods when their config changes, and an **Ingress + Gateway API controller** that renders native Kubernetes routing into Synapse's upstreams.

**What it does:**
- **Config-sync (default)** — hashes every ConfigMap/Secret matching a label selector and stamps the hash onto the Synapse workload, so Kubernetes rolls the pods whenever config content changes (no manual restarts)
- **Ingress / Gateway API mode** (`--ingress-mode`) — reconciles class-matched `Ingress` and Gateway API `HTTPRoute` objects into Synapse's `upstreams.yaml` on a shared volume, hot-reloaded via inotify + `SIGHUP`
- **TLS projection** — projects referenced Ingress/Gateway TLS Secrets into Synapse's certificates directory, operator-owned and hot-reloaded
- **Status & HA** — optionally publishes load-balancer addresses on matched Ingresses and gates shared status writes behind a Lease when running more than one proxy replica
- **Helm-native** — keys off `app.kubernetes.io/name=synapse`, so it plugs straight into Synapse Helm releases

> **Go 1.24+** · any conformant **Kubernetes** cluster · Gateway API CRDs required only for `--gateway-api`

---

## Quick start

### Build

```bash
GOOS=linux GOARCH=amd64 go build -o bin/synapse-operator
```

### Container

```bash
docker build -t ghcr.io/<org>/synapse-operator:latest .
docker push  ghcr.io/<org>/synapse-operator:latest
```

Update `config/manager.yaml` with the pushed image reference.

### Deploy with Kustomize

```bash
kubectl apply -k config
```

Creates the `synapse-os` namespace, ServiceAccount, RBAC, and a single operator replica.

### Deploy with Helm (alongside Synapse)

```bash
helm repo add gen0sec https://helm.gen0sec.com
helm repo update

export GEN0SEC_API_KEY="REPLACE_ME"
helm upgrade --install synapse-stack gen0sec/synapse-stack \
  -n synapse --create-namespace \
  --set global.namespaces.synapse="synapse" \
  --set global.namespaces.operator="synapse-os" \
  --set synapse.image.repository="ghcr.io/gen0sec/synapse" \
  --set synapse.image.tag="latest" \
  --set synapse.synapse.gen0sec.apiKey="$GEN0SEC_API_KEY" \
  --set operator.enabled=true \
  --set operator.image.repository="ghcr.io/<org>/synapse-operator" \
  --set operator.image.tag="latest"
```

Verify, then trigger a config change and watch the rollout:

```bash
kubectl -n synapse-os rollout status deployment/synapse-operator
kubectl -n synapse edit configmap synapse-stack          # change any key
kubectl -n synapse rollout status deployment/synapse-stack
# the pod annotation synapse.gen0sec.com/config-hash updates
```

---

## Modes

The operator runs as **one** of three controllers per process, selected by `--ingress-mode` or `--config-sync`. All share the same manager, health probes, and optional namespace scoping.

> **Config-sync** is the default — it never touches routing, it only forces rollouts when watched config changes.
>
> **Ingress / Gateway API** turns native Kubernetes routing objects into Synapse's `upstreams.yaml` and keeps Synapse hot-reloaded in place.

| | Config-sync | Ingress / Gateway API |
|---|:---:|:---:|
| Flag | _(default)_ | `--ingress-mode` |
| Watches | ConfigMaps + Secrets (by label) | `Ingress` (+ `HTTPRoute` with `--gateway-api`) |
| Action | stamp config hash → roll workload | render `upstreams.yaml` → inotify + `SIGHUP` |
| TLS Secret projection | — | ✅ via `--certs-out` |
| Publishes LB status on Ingress | — | ✅ via `--publish-status-address` |
| One-shot initContainer prime | — | ✅ `--render-once` |
| Multi-replica shared-status HA | — | ✅ `--status-leader-election` |

### Path matching

In `--ingress-mode`, each Ingress/HTTPRoute path is rendered into a Synapse route by path type:

| Source | Rendered as |
|---|---|
| Ingress `Prefix` · Gateway `PathPrefix` | Synapse longest-prefix path (key = the path) |
| Ingress `Exact` · Gateway `Exact` | Approximated as a prefix (Synapse matches longest-prefix) + a warning event |
| Ingress `ImplementationSpecific` + `nginx.ingress.kubernetes.io/use-regex: "true"` | Regex route — `match_expr: http.request.path matches "<regex>"` |
| Gateway `RegularExpression` | Regex route — `match_expr: http.request.path matches "<regex>"` |
| `/.well-known/acme-challenge/*` | `internal_paths` (cert-manager HTTP-01 solver) |

Regex paths are **anchored at the start** (`^`) since they match from the beginning of the request path (the Kubernetes Ingress spec also requires the stored path to begin with `/`, so the leading `^` is supplied by the renderer). The path-map key for a regex route is a unique label; matching is driven entirely by `match_expr`. Header / method / query-param match conditions are not representable in Synapse's host+path model and are dropped with a warning event.

---

## Architecture

```mermaid
flowchart TD
    subgraph K8s[Kubernetes API]
        CM[ConfigMaps / Secrets]
        ING["Ingress / HTTPRoute<br/>(+ Gateway API)"]
        TLS[TLS Secrets]
    end

    subgraph OP[synapse-operator · controller-runtime]
        direction TB
        C1[Config-sync controller]
        C2["Ingress / Gateway controller<br/>(--ingress-mode)"]
    end

    CM --> C1
    ING --> C2
    TLS --> C2

    C1 -->|patch synapse.gen0sec.com/config-hash| WL[Synapse workload<br/>Deployment / DaemonSet / StatefulSet]
    WL -->|rolls pods| SYN[Synapse pods]
    C2 -->|render upstreams.yaml + project certs| SHV[(Shared volume)]
    SHV -->|inotify| SYN
    C2 -.SIGHUP.-> SYN
```

---

## Configuration flags

**Common**

| Flag | Default | Purpose |
|---|---|---|
| `--metrics-bind-address` | `:8080` | Metrics endpoint address |
| `--health-probe-bind-address` | `:8081` | Health probe address |
| `--leader-elect` | `false` | Leader election for the controller manager |
| `--namespace` | _(all)_ | Restrict the watch to one namespace |

**Config-sync mode**

| Flag | Default | Purpose |
|---|---|---|
| `--label-selector` | `app.kubernetes.io/name=synapse` | Selects config sources and workloads |
| `--config-hash-annotation` | `synapse.gen0sec.com/config-hash` | Annotation key for the hash |
| `--ignore-configmap-keys` | `upstreams.yaml` | Comma-separated ConfigMap keys excluded from the hash |
| `--ignore-secret-keys` | _(none)_ | Comma-separated Secret keys excluded from the hash |

**Ingress / Gateway API mode** (`--ingress-mode`)

| Flag | Default | Purpose |
|---|---|---|
| `--render-once` | `false` | One-shot: render `upstreams.yaml` and exit (initContainer prime) |
| `--ingress-class` | `synapse` | `spec.ingressClassName` this controller serves |
| `--upstreams-out` | `/shared/upstreams.yaml` | Path of the rendered upstreams file (sidecar layout) |
| `--upstreams-out-configmap` | _(none)_ | Central layout: write the rendered `upstreams.yaml` to this ConfigMap (`namespace/name`) instead of a file. Only the `upstreams.yaml` key is updated; other keys are preserved. Synapse reloads from its ConfigMap mount. |
| `--resolve-backend-cluster-ips` | `false` | Emit `<clusterIP>:port` instead of `<svc>.<ns>.svc.<cluster-domain>:port`, so Synapse skips backend DNS (falls back to the FQDN for headless / ExternalName / unallocated Services) |
| `--cluster-domain` | `cluster.local` | Cluster DNS domain for backend FQDNs |
| `--certs-out` | _(disabled)_ | Directory to project referenced TLS Secrets into |
| `--gateway-api` | `false` | Also reconcile Gateway API (requires the CRDs) |
| `--publish-status-address` | _(none)_ | IPs/hostnames to publish on matched Ingresses' status |
| `--reload-process-name` | `synapse` | argv0 of the co-located proxy to `SIGHUP` |
| `--status-leader-election` | `false` | Only the Lease holder writes shared status (>1 replica) |
| `--status-leader-election-id` | `synapse-ingress-status` | Lease name for the shared-status election |
| `--leader-election-namespace` | `$POD_NAMESPACE` | Namespace for the shared-status Lease |

**Config-sync mode** (`--config-sync`) — the thin reload sidecar

| Flag | Default | Purpose |
|---|---|---|
| `--watch-configmap` | _(required)_ | ConfigMap to project, as `namespace/name`. Repeatable. |
| `--out-dir` | `/shared` | Directory to project ConfigMap keys into (the volume shared with synapse) |
| `--sync-keys` | _(all)_ | Comma-separated data keys to project |
| `--sync-once` | `false` | One-shot: project once, guarantee `--ensure-files`, exit (initContainer) |
| `--ensure-files` | `upstreams.yaml` | Filenames that must exist in `--out-dir` before synapse starts |
| `--reload-process-name` | `synapse` | argv0 of the co-located proxy to `SIGHUP` |
| `--reload-debounce` | `500ms` | Coalesce SIGHUP bursts |

### Why this mode exists

A ConfigMap *mount* is not a real-time delivery mechanism. kubelet re-projects a mounted ConfigMap on its own sync loop, so a write can take **~60s** to become visible inside the pod. In central mode nothing signals the proxy either: `SignalReload` is disabled whenever `--upstreams-out-configmap` is set, and `upstreams.yaml` sits in `--ignore-configmap-keys` so the config-hash controller deliberately does not roll the pods. Delivery depends entirely on kubelet propagation.

Config-sync reads the ConfigMap **through the API** instead, collapsing that to a single watch event:

```
ConfigMap write -> watch event -> write <out-dir>/<key> -> SIGHUP
```

which lands well inside a second. Synapse is untouched — it still just reads a file and reloads.

### Deployment requirements

- **`shareProcessNamespace: true`** on the pod, so the sidecar can see the synapse PID.
- **`runAsUser: 0`** on the sidecar container. The operator image is distroless nonroot (`USER 65532`) while synapse runs as root, so `syscall.Kill` returns `EPERM` without it and reloads silently stop.
- **A `--sync-once` initContainer.** Not optional: synapse-proxy does a blocking initial read of its upstreams file and, if that read fails, aborts its whole background service *without ever establishing a file watch* — so a pod that starts before the file exists stays deaf to every later update rather than recovering.
- **Metrics/health binds disabled** (the mode forces `0` unless overridden). synapse binds `:8080` for health in the same pod netns, which collides with the operator's metrics default.
- **RBAC**: `configmaps: get,list,watch` in the namespace. It cannot be narrowed to a single ConfigMap — `resourceNames` is ignored for `list`/`watch`, which a watch-based informer requires.

A missing source ConfigMap is **not** an error and never prunes files: leaving the last-good `upstreams.yaml` in place degrades to stale routing, whereas truncating it takes every route down at once.

### Endpoint-backed upstreams

`--resolve-backend-endpoints` (ingress-mode) renders the **ready pod IPs** from EndpointSlices as the server list, instead of a single Service address.

Without it the renderer emits a Service FQDN and synapse resolves it through its in-process DNS cache, which pins the **first A record only**, is configured once at startup, and does not evict on a failed re-resolve — so a replaced pod can keep receiving traffic long after it is gone. Pod IPs are literals, so synapse never resolves them at all, and a pod change re-renders immediately.

Details that matter:

- The port comes from the **EndpointSlice**, not the Service. Pods listen on `targetPort`; pairing a pod IP with the Service port fails on every request.
- All slices for a Service are unioned (by the `kubernetes.io/service-name` label) and the result is **sorted** — API ordering is unstable, and an unsorted list would make every reconcile look like a change.
- `Ready == nil` counts as ready (the API contract for unknown readiness); only an explicit `false` excludes, as does `Terminating`.
- Dual-stack renders IPv4 when any IPv4 endpoint exists, otherwise IPv6. Unioning both would enter each pod twice.
- An empty result **falls back** to ClusterIP/FQDN rather than rendering an empty server list, which would be a 502 for every request to that host.
- Requires `discovery.k8s.io/endpointslices: get,list,watch`. The watch is only registered when the flag is set.

---

## Documentation

| | |
|---|---|
| [Gen0Sec Docs](https://docs.gen0sec.com/) | Product documentation and guides |
| [Synapse](https://github.com/gen0sec/synapse) | The NDR/proxy this operator manages |
| [`config/`](config/) | Kustomize deployment: namespace, ServiceAccount, RBAC, manager |
| [`SECURITY.md`](SECURITY.md) | Security policy and disclosure |

---

## Thank you!

- [Kubernetes SIGs](https://github.com/kubernetes-sigs/controller-runtime) for controller-runtime
- [Kubernetes Gateway API](https://github.com/kubernetes-sigs/gateway-api) for the Gateway API
