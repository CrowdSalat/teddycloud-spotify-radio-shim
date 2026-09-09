# teddycloud-spotify-radio-shim

The shim image is private in GitHub Container Registry. Pulling it requires
authentication with a GitHub token that has read access to the `crowdsalat`
account (or the repository's packages):

```bash
echo "$GITHUB_TOKEN" | podman login ghcr.io -u crowdsalat --password-stdin
podman pull ghcr.io/crowdsalat/teddycloud-spotify-shim:latest
```

Run it like any container image:

```bash
podman run --rm \
  -e SOLOIST_API_KEY=test -e TEDDYCLOUD_URL=http://localhost \
  ghcr.io/crowdsalat/teddycloud-spotify-shim:latest
```