# NVIDIA UFM — architecture and workflow

Background on the real product, for anyone presenting this mock. The
[README](../README.md) covers what the mock implements; this covers what it is
standing in for.

---

## Architecture — who talks to whom

This is the diagram to draw on a whiteboard if the projector dies. Four bands: what
consumes UFM, the surfaces it exposes, the core, and the services that touch the fabric
itself.

```mermaid
flowchart TB
  subgraph CONSUMERS["CONSUMERS"]
    direction LR
    WEBUI["Web UI<br/>Angular SPA"]
    SCHED["Job schedulers<br/>Slurm · LSF · K8s"]
    OBS["Observability<br/>Grafana · Prometheus"]
    SIEM["SIEM / logging<br/>Fluentd · syslog · SNMP"]
    AUTO["Your automation<br/>curl · Python SDK"]
  end

  subgraph SURFACES["SURFACES"]
    direction LR
    REST["REST API<br/>/ufmRest · /ufmRestV2 · /ufmRestV3 (443)"]
    TELEP["Telemetry endpoints<br/>Prometheus / CSV · :9001 · :9002"]
    STREAM["Streaming out<br/>Fluentd · gRPC · syslog · SNMP trap"]
  end

  subgraph CORE["UFM CORE"]
    direction LR
    SERVER["<b>UFM Server</b><br/>fabric model · event &amp; threshold policy<br/>access control · logical elements (PKeys)<br/>reports · fabric validation"]
    HISTDB["History database<br/>SQLite · counters at 5-min granularity<br/>events, alarms, topology snapshots<br/><i>retention is a sizing decision</i>"]
    PLUGINS["Plugin manager<br/>each plugin = a Docker container<br/>lifecycle + REST managed by UFM<br/>NDT · TFS · PDR · SNMP · Consumer …"]
  end

  subgraph FABRIC["FABRIC SERVICES"]
    direction LR
    SM["Subnet Manager<br/>OpenSM: LIDs, routing,<br/>partitions, AR, sweeps"]
    COLLECTOR["Telemetry collector<br/>ibdiagnet-based; reads<br/>port counters over IB"]
    SHARP["SHARP Aggr. Manager<br/>builds reduction trees<br/>for in-network collectives"]
    DEVMGR["Device Manager<br/>SSH + native CLI:<br/>firmware, config, cables"]
  end

  subgraph MANAGED["MANAGED FABRIC"]
    direction LR
    SWITCHES["Quantum switches<br/>managed &amp; unmanaged"]
    HCAS["ConnectX HCAs · BlueField DPUs<br/>one per GPU on a DGX-class node"]
    CABLES["Cables &amp; transceivers<br/><i>the most common fault source</i>"]
  end

  WEBUI -->|"HTTPS · REST"| REST
  SCHED -->|"HTTPS · REST"| REST
  AUTO  -->|"HTTPS · REST"| REST
  COLLECTOR --> TELEP
  TELEP -->|"scrape"| OBS
  STREAM -->|"push"| SIEM

  REST --> SERVER
  SERVER --> STREAM
  SERVER --> HISTDB
  SERVER --> PLUGINS

  SERVER -->|"configures &amp; reads"| SM
  SERVER -->|"samples"| COLLECTOR
  SERVER -->|"acts on"| DEVMGR
  SERVER --> SHARP

  SM        -->|"MADs over the IB fabric itself"| SWITCHES
  COLLECTOR --> HCAS
  SHARP     --> SWITCHES
  DEVMGR    -->|"out-of-band mgmt network (SSH)"| SWITCHES
  SWITCHES  --- CABLES

  linkStyle default stroke:#94a3b8,stroke-width:1.5px
  linkStyle 0,1,2,6,10,14 stroke:#0d9488,stroke-width:3px

  classDef control fill:#ccfbf1,stroke:#0d9488,stroke-width:2px,color:#134e4a
  classDef normal  fill:#f1f5f9,stroke:#94a3b8,color:#334155
  class REST,SERVER,SM,SWITCHES control
  class WEBUI,SCHED,OBS,SIEM,AUTO,TELEP,STREAM,HISTDB,PLUGINS,COLLECTOR,SHARP,DEVMGR,HCAS,CABLES normal
```

**Teal marks the control path** — the chain that can actually change fabric state, from a
REST call down to an OpenSM routing decision. Grey is the observation and out-of-band
path.

Note the split at the bottom: the subnet manager speaks **in-band over InfiniBand itself**,
while device management goes **out-of-band over the regular Ethernet management network**.

---

## What's in the box

If someone asks "so what *is* UFM, as software?", this is the answer. It's a bundle, not a
monolith.

| Component | What it does | Why it matters in the demo |
|---|---|---|
| **UFM Server** | The central Python service holding the fabric model, event and threshold policy, users and roles, reports, and validation logic. | Everything the UI shows is this process's state, served over REST. |
| **OpenSM (SM)** | The open-source subnet manager. Sweeps the subnet, assigns LIDs, computes routing tables, programs switches, enforces partitions. | The mandatory piece. UFM configures and supervises it; it is not UFM's own invention. |
| **Telemetry collector** | `ibdiagnet`-based sampling of per-port counters, exposed on local Prometheus/CSV endpoints. | Source of every number on the dashboard. Two instances — see the flow diagram. |
| **History database** | SQLite store of counters, events, alarms, and topology snapshots. | Enables "show me this port last Tuesday." Also a sizing question customers ask. |
| **Device Manager** | Out-of-band SSH / native CLI to switches: firmware, config, cable and transceiver info. | Explains why UFM wants an Ethernet management network too. |
| **SHARP Aggregation Manager** | Builds and manages the in-network reduction trees on Quantum switches. | The AI-relevant differentiator: collectives run *in* the switch, not just across it. |
| **Web UI** | Angular single-page app. Dashboard, network map, managed elements, events & alarms, telemetry, fabric validation, logical elements, settings. | What you'll actually be clicking. It is a pure REST client — a good point to make. |
| **Plugin framework** | Docker containers whose lifecycle and REST surface UFM manages. | The extensibility story, and where most integrations live. |
| **HA layer** | Active/standby pair with replicated storage over a dedicated dual-link between the two nodes. | Answers "what happens when UFM dies?" before it's asked. |

### The distinction to hold onto

> **UFM is never in the data path.** Not one byte of GPU traffic passes through it. It
> programs the fabric and watches the fabric; the switches forward.

This single sentence answers half the sceptical questions you'll get.

---

## How a counter becomes an action

This is the flow worth walking through slowly, because it's the whole product in one
picture — and because the loop at the bottom is the part that impresses people.

```mermaid
flowchart TB
  PORT["<b>Switch / HCA port</b><br/>symbol errors, link downs,<br/>bytes, packets, temp"]
  COLLECTOR["<b>Telemetry collector</b><br/>primary: ~30 counters / 30 s<br/>secondary: ~120 / 300 s"]
  ENDPOINTS["<b>Local endpoints</b><br/>:9001 primary · :9002 secondary<br/>Prometheus / CSV"]
  CORE["<b>UFM core</b><br/>threshold + event policy<br/>evaluated per port"]
  EVENTS["<b>Events &amp; alarms</b><br/>GUI · email · syslog<br/>SNMP trap"]
  HISTDB["<b>History DB</b><br/>SQLite, 5-min rollup<br/>for trends &amp; forensics"]
  STREAMP["<b>Streaming plugins</b><br/>TFS / EFS / gRPC →<br/>Fluentd, Grafana, ELK"]
  PDR["<b>PDR / ALM plugin</b><br/>automatic remediation"]

  PORT -->|"reads counters in-band"| COLLECTOR
  COLLECTOR --> ENDPOINTS
  ENDPOINTS -->|"scrape"| CORE
  CORE --> EVENTS
  CORE --> HISTDB
  CORE --> STREAMP
  CORE --> PDR
  PDR -->|"isolate or disable the degrading port —<br/>before it stalls a job"| PORT

  linkStyle default stroke:#94a3b8,stroke-width:1.5px
  linkStyle 6,7 stroke:#ea580c,stroke-width:3px

  classDef loop   fill:#ffedd5,stroke:#ea580c,stroke-width:2px,color:#7c2d12
  classDef normal fill:#f1f5f9,stroke:#94a3b8,color:#334155
  class PDR loop
  class PORT,COLLECTOR,ENDPOINTS,CORE,EVENTS,HISTDB,STREAMP normal
```

**The orange path is the closed loop:** UFM doesn't just *report* a bad port, it can take
it out of service automatically via the **PDR** (packet-drop-rate) or **Autonomous Link
Maintenance** plugin.

Note the **two collector instances** — a fast, narrow set for alerting and a slow, wide set
for diagnostics — which is why storage sizing is a real conversation (roughly **100 MB per
hour for a 1,000-node fabric** at defaults).
