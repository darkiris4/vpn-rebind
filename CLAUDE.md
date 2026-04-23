# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
make build          # compile binary → bin/vpn-rebind (CGO_ENABLED=0, trimpath)
make test           # go test -race ./...
make lint           # golangci-lint run ./... (install separately)
make tidy           # go mod tidy
make run ARGS=...   # build + run against the local Docker daemon
make image          # build single-arch Docker image locally
make image-multiarch # build + push multi-arch image (requires docker buildx)
```

Run a single package's tests:
```sh
go test -race ./internal/config/
go test -race ./internal/controller/
```

## Architecture

This is a single-binary Go service that watches the Docker event stream and recreates dependent containers after a VPN provider container restarts.

### Config loading (`internal/config/config.go`)

`config.Load` builds a `Config` from two optional sources applied in order:
1. A YAML file (default `/config/config.yaml`; skip silently if absent)
2. Environment variable overrides (`VPN_REBIND_*`)

A single group can be defined entirely via env vars (`VPN_REBIND_PROVIDER` + `VPN_REBIND_DEPENDENTS` / `VPN_REBIND_LABEL_SELECTOR`) with no file needed. The YAML file and env vars are additive for the top-level settings; env vars always win for `RebindDelay`, `StopTimeout`, and `LogLevel`.

### Event loop (`internal/controller/controller.go`)

`Controller.Run` subscribes to Docker container events filtered to `die/kill/stop/start` actions. The `groupStates` map is keyed by **provider container name** for O(1) event dispatch.

Two-phase trigger prevents spurious rebinds on cold start:
- `onProviderDown` → sets `needsRebind = true`, cancels any pending timer
- `onProviderStart` → only schedules a rebind if `needsRebind` is true (i.e., the controller observed a prior down event)

The rebind is debounced via `time.AfterFunc(rebindDelay, ...)`. If the provider rapid-cycles, each `start` event cancels the previous timer, so only one rebind fires per restart cycle.

### Rebind execution (`internal/controller/rebind.go`)

`Rebirder.RebindGroup` resolves the full dependent list (explicit names ∪ label-discovered names, deduplicated), then calls `rebindContainer` on each one sequentially. Failures are logged and skipped so remaining dependents still get rebound.

`rebindContainer` does a stop → remove → recreate → start cycle. The critical step is in `cloneHostConfig`: after Docker creates a container its `NetworkMode` is stored as `container:<id>` (a stale ID after the provider restarts). This is normalised to `container:<providerName>` before `ContainerCreate`, so Docker resolves the name to the provider's current running instance.

`buildNetworkingConfig` handles the two cases: for `container:*` network mode the returned config is empty (namespace is controlled by HostConfig); for independently networked containers it reconstructs endpoint settings from the live `NetworkSettings`.

### Release

Tag `v*.*.*` to trigger the GitHub Actions workflow (`.github/workflows/release.yml`), which publishes multi-arch images to `ghcr.io/mikechambers/vpn-rebind` and optionally Docker Hub.
