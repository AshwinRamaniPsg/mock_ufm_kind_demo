# Mock NVIDIA UFM

A standalone, self-contained mock of the **NVIDIA UFM Enterprise REST API**, in Go with
no third-party dependencies. Runs as a 7 MB container so you can develop and demo
UFM clients without a real InfiniBand fabric.

---

## Part 1 — What UFM actually is

**UFM (Unified Fabric Manager)** is NVIDIA's management plane for InfiniBand fabrics.
InfiniBand is not Ethernet: it has no distributed control plane. A fabric is governed by a
central **Subnet Manager (OpenSM)** that discovers topology, assigns **LIDs** (local
addressing), and programs switch routing tables. UFM is the product wrapped around that
subnet manager — discovery, telemetry, provisioning, and a REST API.

### The tiers

| Tier | What it adds |
|---|---|
| **UFM Telemetry** | Counter collection and streaming into a TSDB. Monitoring only. |
| **UFM Enterprise** | The one that matters for integration: discovery, provisioning, the **REST API**, web GUI, congestion tracking. |
| **UFM Cyber-AI** | Predictive maintenance, anomaly detection, correlated alerting. |

### The one concept to understand: pkeys

The whole multi-tenant story is **partition keys (pkeys)**. A pkey is an InfiniBand VLAN.
Ports sharing a pkey can talk; ports that don't, cannot — enforced in switch hardware.

- `0x7fff` is the **default management partition** and always exists.
- Membership is `full` (talks to everyone) or `limited` (only talks to `full` members).
- Each partition carries **QoS**: `mtu_limit`, `service_level`, `rate_limit`.

So "provision a tenant onto the fabric" reduces to: *POST the tenant's port GUIDs into a
pkey.* That single call is the heart of every UFM integration, and it is the centre of
this mock.

### How you talk to it

Three prefixes, differing only in authentication:

| Prefix | Auth |
|---|---|
| `/ufmRest` | HTTP basic (`admin:123456` by default) |
| `/ufmRestV2` | Client certificate |
| `/ufmRestV3` | Token — `Authorization: Basic <token>` |

`/ufmRestV3` is what modern NVIDIA control planes use.

### Key endpoints

```
GET  /app/ufm_version                  # version handshake
GET  /app/smconf                       # subnet manager config
GET  /resources/ports                  # discovered ports (GUID, LID, link state)
GET  /resources/pkeys                  # list partitions
POST /resources/pkeys                  # bind GUIDs into a pkey  ← provisioning
GET  /resources/pkeys/{pkey}           # one partition
POST /actions/remove_guids_from_pkey   # unbind
PUT  /resources/pkeys/qos_conf         # set partition QoS
```

---

## Part 2 — The upstream landscape (what I found on GitHub)

### `NVIDIA/infra-controller` — the real mock, and the basis for this one

Public, Apache-2.0, actively developed. It contains **`crates/ufm-mock`** — NVIDIA's own
UFM simulator in Rust (~2,500 lines), landed **2026-08-14** in commit `53b5d05`
*"feat(ufm-mock): Mock of UFM service integrated with machine-a-tron"*.

Its design rationale is [issue #3583](https://github.com/NVIDIA/infra-controller/issues/3583)
(closed, 3/3 sub-issues done). The motivating problem is worth quoting, because it is
probably your problem too:

> *"existing IB tests use an in-memory fabric implementation that bypasses the production
> UFM REST client... it does not exercise authentication, HTTP behavior, serialization,
> response sizes, or the production UFM REST client."*

That is the argument for a REST-level mock over a stubbed interface, and it scales to a
**4,500-tray** simulated deployment.

It runs two ways: as a standalone binary, or hosted inside `machine-a-tron` (their hardware
simulator) — enabled in the `nico-machine-a-tron` Helm chart via `ufmMock.enabled: true`,
with a random auth token generated into a Secret.

**This repo reimplements that REST contract in Go.** Response shapes, partition naming,
LID assignment, error codes, and reconciliation semantics are ported from
`crates/ufm-mock/src/{http,state,inventory}.rs`.

### `Mellanox/ufm_sdk_3.0` — the official plugin SDK

Public. 19 plugins (Grafana, Zabbix, SLURM, gRPC streaming, NDT, PDR), plus REST usage
examples under `scripts/`. Useful as a reference for *how clients call UFM*.

### `ibmgtsim` — NVIDIA's full-fidelity simulator

The SDK contains a `run-ufm-simulator` skill documenting an **all-in-one UFM simulator
container** that runs real OpenSM against a simulated fabric from an `ibdiagnet2.db_csv`
topology — the genuine article, including the web UI.

**Caveat: you almost certainly cannot use it tomorrow.** The images live on
`harbor.mellanox.com` (NVIDIA-internal), it requires `--privileged` + host networking, and
the documented readiness poll allows **up to 40 minutes** to start. It is the right tool
for fabric-behaviour fidelity; it is the wrong tool for a demo with a deadline.

### On "UFM EFS"

**EFS = Events Fluent Streaming**, a UFM plugin that scrapes UFM syslog and streams events
to a Fluentd/Fluent Bit endpoint. It is not related to AWS EFS.

- Source: `Mellanox/ufm_sdk_3.0` → `plugins/ufm_syslog_streaming_plugin`
- Image: `mellanox/ufm-plugin-efs` on Docker Hub
- Config: `[fluent-bit-endpoint]`, `[syslog-destination-endpoint]`, `[streaming]` sections
- Plugin REST: `GET/PUT /plugin/efs/conf`

**Change activity is low.** The plugin's last source commit was **2026-03-24** (`055432b`,
a Helm packaging change), and Docker Hub tags stop at **1.0.0-6, published 2024-05-07**.
Treat EFS as stable/dormant rather than actively evolving. Recent SDK energy has gone into
`ufm-state-mirror` instead.

**This repo now implements the EFS path too** — see Part 5. The real `mellanox/ufm-plugin-efs`
image is amd64-only and last published 2024-05-07, so it will not run on an arm64 kind node;
`./ufm-mock efs` is a Go stand-in with the same config schema and the same wire behaviour.

---

## Part 3 — Running it

### Locally

```bash
go build -o ufm-mock . && ./ufm-mock
./demo.sh                       # scripted 16-step walkthrough
```

### In a pod — one command, from nothing

```bash
./fresh-cluster.sh
```

Creates a dedicated `ufm-demo` kind cluster, builds and loads the image, deploys the mock,
and smoke-tests it. The cluster publishes the mock on **`http://127.0.0.1:9888`** directly,
so no `port-forward` needs to stay running during your demo.

| Flag | Effect |
|---|---|
| *(none)* | Prompts before deleting existing kind clusters — you must type `delete` |
| `--yes` | Skip the prompt |
| `--keep` | Leave other kind clusters alone; only replace `ufm-demo` |
| `--down` | Delete the `ufm-demo` cluster and exit |

> **Destructive by default.** Without `--keep` it deletes *every* kind cluster on the
> machine, not just this one. It lists them and requires confirmation first, and it never
> touches non-kind clusters. Use `--keep` if you have other kind clusters you care about.

### In a pod — manually

The image build pulls **nothing**: the binary is cross-compiled on the host and packaged
into a `FROM scratch` image, so it works offline or behind a slow registry.

```bash
# Match the target node's architecture (arm64 for kind on Apple Silicon).
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/ufm-mock .
docker build --platform linux/arm64 -t ufm-mock:dev .

kind load docker-image ufm-mock:dev --name <cluster>     # or push to your registry
kubectl apply -f deploy/ufm-mock.yaml
kubectl apply -f deploy/ufm-mock-nodeport.yaml
kubectl -n ufm-demo rollout status deploy/ufm-mock
```

> Note your host Go's `GOARCH` may differ from the cluster node's — an amd64 Go toolchain
> on an arm64 Mac is common. `fresh-cluster.sh` detects the node arch and sets `GOARCH`
> for you.

This creates namespace `ufm-demo` with the mock, a Service, a Secret holding the auth
token, a ConfigMap holding the fabric topology, and a `curl` client pod.

Drive it from inside the cluster:

```bash
kubectl -n ufm-demo exec -it deploy/ufm-client -- sh
curl -s -H "Authorization: Basic $UFM_TOKEN" $UFM_URL/ufmRestV3/resources/ports
```

Or from your laptop:

```bash
kubectl -n ufm-demo port-forward svc/ufm-mock 8080:80
UFM_URL=http://127.0.0.1:8080 ./demo.sh
```

Point your own client at `http://ufm-mock.ufm-demo.svc.cluster.local` with
`Authorization: Basic demo-token`.

### Configuration

| Variable | Default | Meaning |
|---|---|---|
| `UFM_MOCK_LISTEN` | `0.0.0.0:9888` | Listen address |
| `UFM_MOCK_AUTH_TOKEN` | `demo-token` | Token for `Authorization: Basic <token>` |
| `UFM_MOCK_VERSION` | `6.18.0` | Reported UFM version |
| `UFM_MOCK_SEED` | – | Path to a topology JSON file |
| `UFM_MOCK_PORTS` | `8` | Synthetic port count when no seed is given |
| `UFM_MOCK_USER` / `UFM_MOCK_PASSWORD` | `admin` / `123456` | Basic-auth pair |

---

## Part 4 — Beyond the REST contract

Three extra endpoints, outside the UFM API, make it useful as a test target.

### Live fabric changes — `POST /admin/inventory`

Push a **complete, versioned inventory snapshot** and the fabric reconciles to it. This
models how the real mock polls `machine-a-tron` sources:

```bash
curl -X POST http://localhost:9888/admin/inventory -d '{
  "inventory_id": "demo-inventory",
  "epoch_id": "epoch-1",
  "generation": 2,
  "machines": [{"mat_id":"mat-a","machine_id":"gpu-node-001",
    "infiniband_ports":[{"guid":"0xb8cef60300000001","state":"down"}]}]}'
```

Semantics ported from upstream:

- The snapshot is **authoritative** — GUIDs it stops advertising are removed from the
  fabric *and* from every partition, and a partition emptied that way is deleted.
- **Generations only move forward** within an epoch. Replaying returns `duplicate`; going
  backwards returns `stale`. Both are ignored.
- A **new `epoch_id`** (the source restarted) resets the generation watermark.
- **LIDs stay stable** across updates, so pkey membership survives a link-state change.

This is how you demo a link failure, a node being drained, or a source restart.

### Fault injection — `/Injection/rules`

Make UFM fail on demand, to show how your client handles an outage:

```bash
curl -X POST http://localhost:9888/Injection/rules -d '{
  "id": "ufm-outage",
  "selector": {"Path": {"method": "GET", "glob": "/ufmRestV3/app/ufm_version"}},
  "action": {"Status": 503}}'

curl -X DELETE http://localhost:9888/Injection/rules   # clear
```

Matching routes return the injected status; everything else stays healthy.

### Metrics — `GET /metrics`

Prometheus format: `ufm_mock_ports`, `ufm_mock_ports_active`, `ufm_mock_partitions`,
`ufm_mock_inventory_sources`, `ufm_mock_requests_total`, `ufm_mock_unauthorized_total`.

---

## Part 5 — Seeing it, alarms, and EFS

### Seeing it

There is no UFM web GUI here, but `GET /ui` serves a live dashboard — ports, partitions,
alarms and events, polling the same REST API as everything else:

```bash
open http://127.0.0.1:9888/ui          # add ?token=<token> if you changed it
```

Plus `kubectl -n ufm-demo logs -f deploy/ufm-mock` and `GET /metrics`.

### Alarms and events

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/app/events` | List events; filters `object_name`, `type`, `category`, `severity` |
| `GET` | `/app/events/{id}` | One event |
| `POST` | `/app/events/external_event` | Raise one event |
| `POST` | `/app/events/external_events` | Raise several |
| `GET` | `/app/alarms` | Active alarms; `?device_id=` to filter |
| `GET` | `/app/alarms/{id}` | One alarm |
| `DELETE` | `/app/alarms?device_id=` | Clear a device's alarms |
| `DELETE` | `/app/alarms` | Clear all (convenience, not in real UFM) |
| `GET`/`PUT` | `/app/syslog` | Syslog config — drives the EFS pipeline |

Raise one:

```bash
curl -X POST -H "Authorization: Basic demo-token" -H "Content-Type: application/json" \
  http://127.0.0.1:9888/ufmRestV3/app/events/external_event -d '{
    "event_id": 331, "name": "Link Down", "severity": "Critical",
    "object_name": "gpu-node-002", "otype": "IBPort", "category": "Hardware",
    "description": "Port b8cef60300000004 went down"}'
```

Modelling rules:

- Only **Warning and above** raise a standing alarm. Info and Debug are events only.
- **Repeats do not stack**: the same `object_name` + `event_type` bumps `event_count`.
- **Clearing alarms never deletes events** — history stays as an audit trail.
- `duration` is computed at read time from when the alarm was first raised.

### EFS — Events Fluent Streaming

```
mock UFM  --syslog/UDP-->  EFS plugin  --Fluent Forward-->  collector
  :9888                      :5140                            :24225
```

All three roles are the same binary:

```bash
./ufm-mock          # the UFM
./ufm-mock efs      # the EFS plugin  (GET/PUT /plugin/efs/conf, /plugin/efs/stats)
./ufm-mock sink     # a Fluentd-compatible collector that logs what it receives
```

Wire it up — the two calls that matter:

```bash
# 1. EFS: enable streaming (partial PUT merges, as upstream does)
curl -X PUT http://127.0.0.1:8989/plugin/efs/conf \
  -d '{"streaming":{"enabled":true},"fluent-bit-endpoint":{"enabled":true}}'

# 2. UFM: send syslog to EFS, at WARNING and above
curl -X PUT -H "Authorization: Basic demo-token" -H "Content-Type: application/json" \
  http://127.0.0.1:9888/ufmRestV3/app/syslog -d '{
    "active": true, "destination": "127.0.0.1:5140",
    "level": "WARNING", "ufm_log": true, "events_log": true}'
```

Now every Warning-or-worse event leaves UFM as an RFC 3164 datagram, EFS forwards it over
the **Fluent Forward protocol** (MessagePack, hand-encoded — no dependencies), and the
collector prints it. `./demo-efs.sh` walks all of it.

Because it is real Fluent Forward, you can point EFS at an actual Fluentd or Fluent Bit
instead by setting `EFS_FLUENT_HOST` / `EFS_FLUENT_PORT`, or via `PUT /plugin/efs/conf`.

The severity filter is the thing to demo: set `level` to `WARNING`, raise an `Info` event,
and show that it appears in `/app/events` but never reaches the collector.

---

## Fidelity

Verified against the assertions in NVIDIA's own `crates/ufm-mock` tests:

- GUIDs render as 16 lowercase hex digits; ports are named `<guid>_1`.
- pkeys render as `0x7fff`; a bare number in a request is decimal, `0x`-prefixed is hex.
- Creating a partition via the API names it `api_pkey_0x1`; the default is `management`.
- Only `0x7fff` reports a top-level `membership`.
- `guids: []` is present-but-empty when `guids_data=true`, never omitted.
- New partitions start at QoS `{mtu_limit: 2, service_level: 0, rate_limit: 2.5}`.
- Binding an unknown GUID fails the **whole** request with 404 — no partial binds.
- `GET` on an unknown pkey returns **200 with `{}`**, not 404.
- Unbinding the last GUID deletes a non-default partition.

`go test ./...` covers all of the above.

### Where it deliberately differs

- **Single-owner ports.** Upstream tracks multiple inventory *candidates* per GUID and
  resolves conflicts by lowest `inventory_id`. This mock assigns each GUID one owner —
  equivalent for single-source use, simpler to read.
- **Push, not poll.** Upstream polls configured HTTP sources on an interval with a failure
  grace period. Here you `POST` snapshots when you want a change, which is better for a
  scripted demo.
- **No TLS.** Upstream supports TLS and client certs. Terminate at an Ingress if you need
  HTTPS.
- **No plugin surface**, no telemetry counters, no web GUI, no real OpenSM. For fabric
  *behaviour* fidelity you want `ibmgtsim`; this models the *API contract*.

---

## Layout

```
main.go            HTTP routing, auth, fault injection, metrics, seeding
events.go          events, alarms, syslog configuration
syslog.go          RFC 3164 emission
efs.go             EFS forwarder, Fluent Forward codec, collector
ui.go              the /ui dashboard
fabric.go          fabric state: ports, partitions, inventory reconciliation
fabric_test.go     contract tests
demo.sh            16-step fabric + pkey walkthrough
demo-efs.sh        13-step alarms + EFS walkthrough
fresh-cluster.sh   nuke kind, rebuild a cluster, deploy, smoke-test
Dockerfile         static build → distroless (7 MB)
deploy/
  kind-cluster.yaml       cluster config, publishes 30888 → localhost:9888
  ufm-mock.yaml           namespace, Secret, ConfigMap topology, Deployment, Service, client
  ufm-mock-nodeport.yaml  NodePort so the host port mapping reaches the mock
  ufm-efs.yaml            EFS plugin + Fluentd-compatible collector
docs/
  NVIDIA_UFM_WORKFLOW.md  real-product architecture and telemetry→action flow
```

## References

- [docs/NVIDIA_UFM_WORKFLOW.md](docs/NVIDIA_UFM_WORKFLOW.md) — how the real UFM is put
  together, and the closed loop from a port counter to an isolated port

- [NVIDIA UFM product page](https://www.nvidia.com/en-us/networking/infiniband/ufm/)
- [UFM Enterprise REST API Guide v6.25.1](https://networking-docs.nvidia.com/ufmenterpriserestapi/6251)
- [NVIDIA/infra-controller](https://github.com/NVIDIA/infra-controller) — `crates/ufm-mock`
- [infra-controller issue #3583](https://github.com/NVIDIA/infra-controller/issues/3583) — simulator design
- [Mellanox/ufm_sdk_3.0](https://github.com/Mellanox/ufm_sdk_3.0) — plugin SDK, EFS plugin
