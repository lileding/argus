# Roadmap

Argus's core architecture is in place. The next phase is
**productization** — making it usable beyond a single user / single
chat. This file captures the candidate themes and the active workstream.

## Themes

| # | Theme | Status | Notes |
|---|---|---|---|
| 1 | Long-running async task health | Deferred | Not urgent; revisit when we hit it in practice |
| 2 | **Feishu multi-channel (channel system)** | **Active** | See "Channel System" below |
| 3 | Multi-IM + `feishu` crate decoupling | Deferred | Architecturally simplest but lowest immediate need |
| 4 | Web admin console | Open | Originally intended to ease #2; revisit after #2 ships |

---

## Channel System (active)

Make Argus serve multiple logical "users" via a static channel
configuration, instead of treating the whole instance as one user.

### Concepts

| Concept | Definition |
|---|---|
| **Sink** | An IM endpoint provided by a Gateway. Feishu: each DM is a sink, each group is a sink. |
| **Channel** | Argus's "user" / tenant. Permanent under static config. |
| **Sink → Channel** | Many-to-one mapping. Unmapped sinks fall back to the default channel. |
| **Channel ↔ Agent** | 1:1. Each channel has its own Agent system. |

### Semantics

- **Channel = tenant**: `task` / `cron` / `memory` / `documents` are
  isolated per channel. Cross-channel access is not allowed.
- **Channel = user**: people sharing a sink (e.g. group members) are
  the same "user" from Argus's perspective. No per-person identity.
- **Concurrency**: channels run fully in parallel; within a channel,
  sync messages are FIFO and async tasks are parallel (current
  intra-channel semantics).
- **Routing**: notifications travel back to the sink the originating
  message came from.
- **Lifecycle**: under static config, channels are permanent. No
  runtime creation/deletion, no Feishu-event-driven mutations.

### Configuration shape

Channel-centric — sinks listed under the channel that owns them:

```toml
[channel.work]
sinks = [
  { gateway = "feishu", kind = "p2p", id = "ou_alice" },
  { gateway = "feishu", kind = "group", id = "oc_eng" },
]

[channel.family]
sinks = [
  { gateway = "feishu", kind = "p2p", id = "ou_mom" },
]
```

The **default channel is implicit** — it always exists, has no name,
cannot be configured. Any sink not listed under a configured channel
is routed to it.

### Out of scope (for this phase)

- Per-channel model / skill / prompt overrides (could come later as
  part of #4 Web admin)
- User identity within a channel (groups treat all members as one user)
- Dynamic channel creation / Feishu-event-driven channel discovery
- Cross-channel resource access
