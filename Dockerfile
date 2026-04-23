# syntax=docker/dockerfile:1

# ---- Build stage ----
FROM golang:1.22-alpine AS builder

WORKDIR /src

# Download dependencies first so they are cached separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build a statically-linked binary — no external runtime dependencies needed.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION:-dev}" \
    -o /vpn-rebind \
    ./cmd/vpn-rebind

# ---- Final stage ----
# Use a minimal distroless image for the smallest possible attack surface.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /vpn-rebind /vpn-rebind

# Config file mount point (optional — env vars work without a file).
VOLUME ["/config"]

ENTRYPOINT ["/vpn-rebind"]
