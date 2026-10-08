# teddycloud-spotify-radio-shim

The shim image is private in GitHub Container Registry. Pulling it requires
authentication with a GitHub token that has read access to the `crowdsalat`
account (or the repository's packages):

```bash
echo "$GITHUB_TOKEN" | podman login ghcr.io -u crowdsalat --password-stdin
podman pull ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:latest
```

Run it like any container image:

```bash
podman run --rm \
  -e SOLOIST_API_KEY=test -e TEDDYCLOUD_URL=http://localhost \
  ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:latest
```

## Local test bench

The whole figurine cycle can be run on one machine, with no Toniebox and no
deployed teddyCloud. Three processes:

| | |
|---|---|
| **teddyCloud** | real, in a container, so the RTNL→SSE translation and the ffmpeg pull are real rather than mocked |
| **the shim** | the code under test |
| **a simulated box** | [`cmd/mock-toniebox`](cmd/mock-toniebox), which speaks the box side of the protocol |

```bash
make bench-up                      # starts all three, prints what to run next
./scripts/cycle.sh place           # put a figurine on the box
./scripts/cycle.sh lift            # take it off
./scripts/cycle.sh cycle 10        # 10 place/lift cycles, with the measurement table
./scripts/cycle.sh report          # measurements so far, as JSON
./scripts/cycle.sh log shim        # the shim's control path
./scripts/cycle.sh down            # tear the bench down
```

`place` and `lift` are the two that matter. They produce exactly the signals the
shim's control path is built from — `TagValid` becomes `play`, `playback=stopped`
becomes `pause` — so a cycle here drives the same code a real figurine swap does.
That makes the silence-after-swap defect reproducible on a laptop.

Ports, so nothing collides with a shim already on 8080: teddyCloud's web listener
keeps **8080** (`/api/sse`, GUI), the shim takes **8090**, the box control surface
**9095**.

Real Soloist pairing is the default. The shim needs PulseAudio or PipeWire-pulse
for the recorder, so on a host with no sound stack use the synthesized tone
instead — the transport and control path are identical, only the audio differs:

```bash
make bench-up STATIC_STREAM=1
```

Details, and the protocol notes behind them, are in
[`docs/mock-toniebox-design.md`](docs/mock-toniebox-design.md).