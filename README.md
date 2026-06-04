# teddycloud-spotify-radio-shim

A bridge between [Teddycloud](https://github.com/toniebox-reverse-engineering/teddycloud) and Spotify. It translates physical Toniebox interactions (figurine placed/lifted, ear slaps) into Spotify playback commands and streams the audio back to Teddycloud.

> **Status:** Pre-implementation — documentation and project skeleton only. No working features yet.

## How It Works

The shim sits between Teddycloud and Spotify with two data paths:

```
Audio:    Toniebox → Teddycloud → Shim /stream ← FIFO ← go-librespot ← Spotify
Control:  Toniebox → Teddycloud → SSE → Shim → REST API → go-librespot
```

Teddycloud configures a figurine to point at the shim's `/stream` endpoint with a Spotify URI. When the Toniebox requests audio, the shim tells [go-librespot](https://github.com/devgianlu/go-librespot) to play that URI and pipes the decoded audio back. Physical events (figurine lifted = pause, ear slap = skip) are received via Teddycloud's SSE feed and forwarded as API calls.

## Documentation

```
docs/
├── PRD.md                 Product requirements
├── ARCHITECTURE.md        Solution design, data flow, components
├── PLAN.md                Phased implementation plan (9 phases)
├── adr/                   Architecture Decision Records
│   ├── 001-spotify-engine.md       Why go-librespot
│   ├── 002-container-pattern.md    Why subprocess, not sidecar
│   ├── 003-control-channel.md      Why SSE, not MQTT
│   ├── 004-playback-control.md     Why unified API control
│   └── 005-fifo-backpressure.md    FIFO reader design and deadlock prevention
└── research/
    └── go-librespot-pipe-deadlock.md   Upstream bug analysis
```

## Building

```bash
make build                    # build the shim binary
make test                     # run tests
make lint                     # go vet
make container                # build multi-arch container image
```

Requires Go 1.21+ for local builds. The container image builds go-librespot v0.7.3 from source (requires Go 1.25, handled inside the build stage).

## License

TBD
