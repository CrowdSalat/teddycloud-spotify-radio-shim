# ADR-003: SSE over MQTT for Teddycloud Event Ingestion

**Status:** Accepted  
**Date:** 2026-06-04

## Context

The shim must receive real-time hardware events from Teddycloud (figurine placed/lifted, ear slaps, knocks) to map them to Spotify playback commands. Teddycloud emits events through two parallel channels:

- **SSE** at `GET /api/sse` — long-lived HTTP chunked-transfer, JSON payloads, keepalive every 15s, 60s timeout, max 8 concurrent subscribers.
- **MQTT** to a configurable broker — HA Discovery entities with stable topic structure documented in `MQTT_CONTROL.md` (`KnockForward`, `KnockBackward`, `TagValid`, `Playback`, etc.).

Both channels are fed from the same `tbs_*` C functions in Teddycloud, so they carry identical events.

## Options Considered

1. **SSE** — Zero infrastructure. One HTTP GET. Client-side filtering. Manual reconnect.
2. **MQTT** — Requires a broker (Mosquitto, etc.). Broker-side topic filtering. Protocol-native reconnection with QoS. Stable documented entity contract.

## Decision

**SSE as default, with a pluggable `ControlChannel` interface so MQTT can be swapped in via `CONTROL_CHANNEL=mqtt` env var.**

## Rationale

| Criterion | SSE | MQTT |
|---|---|---|
| Infrastructure | ✅ Zero — just HTTP to Teddycloud | ⚠️ Requires running broker |
| Deployment complexity | ✅ One env var (`TEDDYCLOUD_URL`) | ⚠️ Broker URL, port, creds, topic prefix |
| OpenShift footprint | ✅ No extra pods/PVCs | ⚠️ Broker pod + PVC if deploying new |
| Event contract stability | ⚠️ Inline C strings, undocumented | ✅ `MQTT_CONTROL.md` |
| Reconnection | ⚠️ ~15 lines of manual reconnect | ✅ Protocol-native |
| Filtering | ⚠️ Client-side (all events to all subscribers) | ✅ Topic-based server-side |

For a `replicas: 1` singleton processing <10 events/minute, SSE's limitations are trivial. Reconnect is a loop with backoff. Client-side filtering is a switch statement. The event contract risk is mitigated by a mapping layer (SSE event name → internal action enum) — if Teddycloud renames events, only the mapping table changes.

MQTT is preferred **when a broker already exists** (e.g., Home Assistant integration). The interface design makes this a configuration change, not a code change.

## Consequences

- The shim defines a `ControlChannel` interface with `Events() <-chan Event`. Two implementations: `SSEChannel`, `MQTTChannel`.
- SSE event names must be verified against Teddycloud source (especially tag-removed events from `tbs_tag_removed()`).
- SSE occupies 1 of Teddycloud's 8 subscriber slots.
- The SSE connection to Teddycloud should use TLS if crossing OpenShift namespace boundaries.
