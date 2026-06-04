# ADR-002: Single-Container Subprocess over Multi-Container Sidecar

**Status:** Accepted  
**Date:** 2026-06-04

## Context

The PRD mandates that go-librespot and the shim run inside a single OpenShift Pod (`replicas: 1`). Two patterns exist for co-locating them: a multi-container sidecar (two containers sharing a Pod network namespace and an `emptyDir` volume for the FIFO) or a single container where the shim spawns go-librespot as a child process.

## Options Considered

1. **Multi-container sidecar** — Two containers, shared `emptyDir` for FIFO, shared `localhost` for API. Independent restarts, per-container resource limits, independent image builds.
2. **Single-container subprocess** — One container. Shim is PID 1 (via `tini`), spawns go-librespot as a child. FIFO in `/tmp`. One Dockerfile, one image.

## Decision

**Single-container subprocess.**

## Rationale

The dominant risk is **FIFO startup ordering**. The named FIFO reader must be open before go-librespot's pipe driver opens the writer side (`O_WRONLY|O_NONBLOCK` — errors if no reader). In the sidecar model, OpenShift provides no container startup ordering; an init container or retry loop is required, adding fragile choreography. In the subprocess model, the shim controls sequencing deterministically:

```
mkfifo /tmp/spotify.fifo → open reader fd → spawn go-librespot → wait for API readiness
```

No race, no init container, no retry loop.

| Criterion | Sidecar | Subprocess | Winner |
|---|---|---|---|
| FIFO startup race | ⚠️ Init-container choreography | ✅ Sequential, in-process | Subprocess |
| Build/deploy complexity | ⚠️ Two images, two pipelines | ✅ Single image, single pipeline | Subprocess |
| OpenShift YAML | ⚠️ Multi-container + shared volumes | ✅ Simple single-container Deployment | Subprocess |
| Crash recovery | ✅ kubelet restarts container | ⚠️ Shim must supervise child | Sidecar |
| Resource granularity | ✅ Per-container limits | ⚠️ Shared limits | Sidecar (marginal) |

Crash recovery in the subprocess model is ~30 lines of Go: monitor `cmd.Wait()`, restart with backoff. For a `replicas: 1` singleton, the sidecar's advantages (independent restarts, per-container limits) don't justify the startup-ordering complexity.

## Consequences

- The shim must act as a process supervisor: forward signals, reap zombies (use `tini` as PID 1), restart go-librespot on crash with exponential backoff.
- Upgrading go-librespot requires rebuilding the shim image (coupled release cycle). Acceptable for this project's scope.
- Single `resources` block in the Deployment covers both processes.
