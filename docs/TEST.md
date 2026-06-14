# Testing

## Unit Tests

```bash
make test   # runs go test -v ./...
make lint   # runs golangci-lint
```

---

## Integration Tests

Integration tests run the full shim inside a container against a real Spotify
session. They require a pre-authenticated go-librespot session stored on the
host. Phase 1 (subprocess lifecycle) runs without auth; Phases 3 and 4 require
it.

### One-time setup — authenticate and persist the session

**1. Create the config directory on the host:**

```bash
mkdir -p /private/tmp/shim-config
```

**2. Start a container with the config dir mounted:**

```bash
podman run -d --name shim-auth --platform linux/amd64 \
  -p 8080:8080 \
  -v /private/tmp/shim-config:/config:Z \
  shim-integration-test
```

**3. Watch the logs until the Spotify auth URL appears:**

```bash
podman logs -f shim-auth 2>&1 | grep -i "accounts.spotify.com"
```

**4. Open the `https://accounts.spotify.com/authorize?...` URL in a browser,**
complete the Spotify login, then copy the redirect URL from the browser address
bar. It looks like:

```
http://127.0.0.1:<PORT>/login?code=AQD...
```

**5. Feed the redirect URL into the container:**

```bash
podman exec shim-auth curl -s "http://127.0.0.1:<PORT>/login?code=<CODE>"
# Expected response: Go back to go-librespot!
```

**6. Verify the session is saved:**

```bash
podman exec shim-auth curl -s http://localhost:3678/status | python3 -m json.tool
ls -la /private/tmp/shim-config/
# state.json should exist and be non-empty
```

**7. Stop and remove the auth container:**

```bash
podman rm -f shim-auth
```

The session is now persisted in `/private/tmp/shim-config/state.json` on the
host and survives container restarts.

---

### Running the integration tests

**Build the test image:**

```bash
podman build --platform linux/amd64 -f Containerfile -t shim-integration-test .
```

**Run all phases (requires auth session):**

```bash
make test-integration CONFIG_DIR=/private/tmp/shim-config
```

**Run Phase 1 only (no auth required):**

```bash
make test-integration
```

**Skip the image build (reuse existing image):**

```bash
./scripts/test-integration.sh --skip-build --config-dir /private/tmp/shim-config
```

---

### What the integration tests cover

| Phase | What is tested | Auth required |
|---|---|---|
| 1 | go-librespot starts, port reachable, restarts on crash, clean shutdown | No |
| 3 | `/stream` returns HTTP 200 + `audio/wav`, audio bytes flow, valid WAV header | Yes |
| 4 | Hot-swap: second `/stream` request kills first connection, second gets data | Yes |
