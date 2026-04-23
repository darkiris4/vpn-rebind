# vpn-rebind

An event-driven Docker control-plane controller that enforces VPN network namespace correctness for container groups.

When a VPN provider container (e.g. [Gluetun](https://github.com/qdm12/gluetun)) restarts, its Linux network namespace is replaced. Dependent containers that were attached via `network_mode: container:<provider>` are now silently orphaned in a stale namespace. **vpn-rebind** watches for this event and deterministically recreates each dependent container so it attaches to the fresh namespace — no health-check polling, no manual restarts, no silent fallback to host networking.

## How it works

```
Docker daemon ──events──► vpn-rebind controller
                               │
                  provider die │ → mark group "needs rebind"
                  provider start│ → schedule debounced rebind
                               │
                         [rebind_delay]
                               │
                        for each dependent:
                          stop → remove → recreate → start
                               │
                        (NetworkMode normalised from
                         container:<old-id>  →  container:<name>
                         so Docker resolves to new namespace)
```

The controller starts **without** performing any rebind. It only acts after it has observed a provider go down **and** come back up, so a normal `docker compose up` startup is unaffected.

## Quick start

### Environment variables (no config file needed)

```yaml
services:
  vpn-rebind:
    image: ghcr.io/darkiris4/vpn-rebind:latest
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    environment:
      - VPN_REBIND_PROVIDER=gluetun
      - VPN_REBIND_DEPENDENTS=qbittorrent,prowlarr
```

### YAML config file

Mount a config file to `/config/config.yaml`:

```yaml
rebind_delay: 5s

groups:
  - name: gluetun
    provider: gluetun
    dependents:
      - qbittorrent
      - prowlarr
    label_selector:
      vpn.required: "true"
      vpn.provider: gluetun
```

```yaml
services:
  vpn-rebind:
    image: ghcr.io/darkiris4/vpn-rebind:latest
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - ./vpn-rebind:/config:ro
```

See [`config.example.yaml`](config.example.yaml) for all options and [`docker-compose.example.yml`](docker-compose.example.yml) for a full Gluetun + qBittorrent + Prowlarr stack.

## Configuration reference

### YAML fields

| Field | Default | Description |
|---|---|---|
| `rebind_delay` | `3s` | How long to wait after the provider starts before rebinding. Gives the VPN tunnel time to establish. |
| `stop_timeout` | `10s` | Graceful stop timeout before force-removing each dependent. |
| `log_level` | `info` | Verbosity: `debug` \| `info` \| `warn` \| `error` |
| `groups[].name` | required | Human-readable label shown in log output. |
| `groups[].provider` | required | Exact Docker container name of the VPN provider. Must match `container_name:` in Compose. |
| `groups[].dependents` | | Explicit list of dependent container names. |
| `groups[].label_selector` | | Discover dependents dynamically by matching Docker labels. All key=value pairs must match. |

At least one of `dependents` or `label_selector` is required per group.

### Environment variables

| Variable | Equivalent YAML | Notes |
|---|---|---|
| `CONFIG_PATH` | — | Path to config file. Default: `/config/config.yaml` |
| `VPN_REBIND_PROVIDER` | `groups[0].provider` | Defines a single `default` group when no config file is present. |
| `VPN_REBIND_DEPENDENTS` | `groups[0].dependents` | Comma-separated list: `qbittorrent,prowlarr` |
| `VPN_REBIND_LABEL_SELECTOR` | `groups[0].label_selector` | Comma-separated key=value pairs: `vpn.required=true,vpn.provider=gluetun` |
| `VPN_REBIND_DELAY` | `rebind_delay` | Duration string: `5s`, `10s` |
| `VPN_REBIND_STOP_TIMEOUT` | `stop_timeout` | Duration string |
| `VPN_REBIND_LOG_LEVEL` | `log_level` | `debug` \| `info` \| `warn` \| `error` |

### Label-based opt-in

Add labels to dependent containers for automatic discovery:

```yaml
services:
  qbittorrent:
    labels:
      vpn.required: "true"
      vpn.provider: gluetun
```

Labels can be combined with the explicit `dependents` list — the controller deduplicates.

### Multiple providers

```yaml
groups:
  - name: gluetun
    provider: gluetun
    dependents: [qbittorrent, prowlarr]

  - name: wireguard
    provider: wireguard
    label_selector:
      vpn.provider: wireguard
```

## Docker permissions

vpn-rebind needs read-write access to the Docker socket to watch events and recreate containers:

```yaml
volumes:
  - /var/run/docker.sock:/var/run/docker.sock:ro   # :ro is sufficient for event watching
```

> **Note:** `:ro` prevents vpn-rebind from creating new containers. For full rebind functionality, mount the socket **without** `:ro`:
> ```yaml
> volumes:
>   - /var/run/docker.sock:/var/run/docker.sock
> ```

## Docker Compose compatibility

vpn-rebind recreates containers directly via the Docker API, preserving the full `ContainerConfig` and `HostConfig` from the original container. This means:

- All environment variables, volume mounts, labels, and restart policies are preserved.
- After vpn-rebind recreates a container, `docker compose up` will detect a config hash mismatch (because the Compose hash label was set at original creation time). Running `docker compose up -d` will reconcile this by recreating the container once more under Compose management.
- This is a one-time reconciliation cost after a VPN provider restart event; normal operations are unaffected.

## Building from source

```sh
# Requires Go 1.22+
go mod tidy
make build

# Build Docker image
make image

# Multi-arch push (requires buildx)
make image-multiarch
```

## Releases

Images are published automatically on every `v*.*.*` tag via GitHub Actions to:

- `ghcr.io/darkiris4/vpn-rebind:<version>`

Tags follow [semver](https://semver.org/): `v1.2.3`, `v1.2`, `v1`, and `latest` are published simultaneously.

To publish a release:

```sh
git tag v1.0.0
git push origin v1.0.0
```
