# teddycloud-spotify-radio-shim

The shim image is published publicly in GitHub Container Registry — pull it
like any other image, no login required:

```bash
podman pull ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:latest
```

The image ships no secrets and no Spotify binaries: `SOLOIST_API_KEY` is passed
in at runtime, and Soloist is downloaded on first start.

Run it like any container image:

```bash
podman run --rm \
  -e SOLOIST_API_KEY=test -e TEDDYCLOUD_URL=http://localhost \
  ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:latest
```

## Local test bench

The whole figurine cycle can be run on one machine, with no Toniebox. Three
containers on one network:

| | |
|---|---|
| **teddyCloud** | real, so the RTNL→SSE translation and the ffmpeg pull are real rather than mocked |
| **the shim** | the code under test |
| **[`teddycloud-mock-toniebox`](https://github.com/CrowdSalat/teddycloud-mock-toniebox)** | a simulated box, published as an image |

```bash
make bench-up                      # starts all three, prints what to run next
./scripts/cycle.sh place           # put a figurine on the box
./scripts/cycle.sh lift            # take it off
./scripts/cycle.sh cycle 10        # 10 place/lift cycles, with the measurement table
./scripts/cycle.sh report          # measurements so far, as JSON
./scripts/cycle.sh log shim        # the shim's control path
make bench-down                    # tear the bench down
```

`place` and `lift` are the two that matter. They produce exactly the signals the
shim's control path is built from — `TagValid` becomes `play`, `playback=stopped`
becomes `pause` — so a cycle here drives the same code a real figurine swap does.
That makes the silence-after-swap defect reproducible on a laptop.

The rig uses the **newest published shim**, so it tests what users actually pull.
To test a local build instead, point the compose file at it:

```bash
make container-build
BENCH_SHIM_IMAGE=localhost/teddycloud-spotify-shim:dev make bench-up
```

`STATIC_STREAM` defaults to `1` in the rig: the shim serves a synthesized tone and
needs no audio server, so the bench works unattended with no Spotify session. The
transport and control path are identical either way. For real playback, set
`STATIC_STREAM=0` and pair — which needs PulseAudio or PipeWire-pulse in the
container.

Ports: teddyCloud keeps **8080** (`/api/sse`, GUI) and **8443** (box API), the
shim takes **8090**, the box control surface **9095**. Container-to-container
traffic goes over the compose network by service name, so teddyCloud's ffmpeg
reaches the shim at `http://shim:8080/stream` rather than through a published
port.