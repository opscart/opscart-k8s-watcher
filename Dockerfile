# ── Stage 1: build ────────────────────────────────────────────────────────────
FROM golang:1.25 AS builder

WORKDIR /src

ARG TARGETOS
ARG TARGETARCH

# Cache module downloads separately from source so rebuilds are fast.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build \
      -ldflags="-s -w" \
      -trimpath \
      -o /opscart-dashboard \
      ./cmd/opscart-dashboard

# ── Stage 2: minimal runtime ──────────────────────────────────────────────────
FROM scratch

# TLS root certs — needed for HTTPS to the k8s API server when the cluster
# uses a publicly-signed certificate (most cloud-managed clusters do).
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY --from=builder /opscart-dashboard /opscart-dashboard

EXPOSE 8080

# Run as nobody (uid 65534) — matches the pod securityContext below.
USER 65534

ENTRYPOINT ["/opscart-dashboard"]
