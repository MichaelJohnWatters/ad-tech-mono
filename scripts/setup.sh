#!/bin/bash
# Setup script for the Ad Tech Platform.
# Installs all prerequisites and starts the local K8s cluster.
#
# Usage: ./scripts/setup.sh
#
# Prerequisites: macOS with Homebrew installed.

set -e

echo "=== Ad Tech Platform Setup ==="
echo ""

# --- Check for Homebrew ---
if ! command -v brew &> /dev/null; then
    echo "Homebrew not found. Install it first:"
    echo '  /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"'
    exit 1
fi

# --- Install Go ---
if ! command -v go &> /dev/null; then
    echo "[1/7] Installing Go..."
    brew install go
else
    echo "[1/7] Go: $(go version | awk '{print $3}')"
fi

# --- Install Colima (container runtime + K8s) ---
if ! command -v colima &> /dev/null; then
    echo "[2/7] Installing Colima..."
    brew install colima
else
    echo "[2/7] Colima: $(colima version 2>/dev/null | head -1 || echo 'installed')"
fi

# --- Install kubectl ---
if ! command -v kubectl &> /dev/null; then
    echo "[3/7] Installing kubectl..."
    brew install kubectl
else
    echo "[3/7] kubectl: $(kubectl version --client --short 2>/dev/null || echo 'installed')"
fi

# --- Install Tilt ---
if ! command -v tilt &> /dev/null; then
    echo "[4/7] Installing Tilt..."
    brew install tilt-dev/tap/tilt
else
    echo "[4/7] Tilt: $(tilt version 2>/dev/null | head -1 || echo 'installed')"
fi

# --- Install Buf (protobuf toolchain) ---
if ! command -v buf &> /dev/null; then
    echo "[5/7] Installing Buf..."
    brew install bufbuild/buf/buf
else
    echo "[5/7] Buf: $(buf --version 2>/dev/null || echo 'installed')"
fi

# --- Install golangci-lint ---
if ! command -v golangci-lint &> /dev/null; then
    echo "[6/7] Installing golangci-lint..."
    brew install golangci-lint
else
    echo "[6/7] golangci-lint: $(golangci-lint version --short 2>/dev/null || echo 'installed')"
fi

# --- Install d2 (diagram tool) ---
if ! command -v d2 &> /dev/null; then
    echo "[7/7] Installing d2..."
    brew install d2
else
    echo "[7/7] d2: $(d2 --version 2>/dev/null || echo 'installed')"
fi

echo ""

# --- Optional: k6 for performance testing ---
if ! command -v k6 &> /dev/null; then
    echo "[optional] Installing k6 (performance testing)..."
    brew install k6
fi

# --- Start Colima with K8s ---
echo ""
echo "--- Starting Colima with Kubernetes ---"
if colima status 2>/dev/null | grep -q "Running"; then
    echo "Colima already running."
else
    echo "Starting Colima (4 CPUs, 8GB RAM, K8s enabled)..."
    echo "This may take a few minutes on first run."
    colima start --kubernetes --cpu 4 --memory 8 --disk 60
fi

# --- Configure Docker Hub pull-through cache ---
# Routes all docker.io/* pulls through the in-cluster registry-mirror Pod
# (deployed by Tilt via k8s/base/registry-mirror/). First pull populates
# the PVC; subsequent pulls (including across `colima stop`/`start`) hit
# the cache and survive Docker Hub outages.
#
# Lives here rather than in registries.yaml because Colima runs k3s with
# --docker, which delegates pulls to the Docker daemon (registries.yaml
# would be ignored). `colima delete` wipes /etc/docker/daemon.json, so
# this re-applies on every fresh VM.
echo ""
echo "--- Configuring image cache (Docker Hub pull-through mirror) ---"
MIRROR_URL="http://localhost:30500"
CURRENT_DAEMON=$(colima ssh -- sudo cat /etc/docker/daemon.json 2>/dev/null || echo '{}')
if echo "$CURRENT_DAEMON" | python3 -c "
import json, sys
d = json.load(sys.stdin)
sys.exit(0 if d.get('registry-mirrors') == ['$MIRROR_URL'] else 1)
" 2>/dev/null; then
    echo "Mirror already configured in /etc/docker/daemon.json."
else
    echo "Patching /etc/docker/daemon.json to use mirror at $MIRROR_URL..."
    UPDATED_DAEMON=$(echo "$CURRENT_DAEMON" | python3 -c "
import json, sys
d = json.load(sys.stdin)
d['registry-mirrors'] = ['$MIRROR_URL']
d['insecure-registries'] = ['localhost:30500']
print(json.dumps(d, indent=2))
")
    echo "$CURRENT_DAEMON" | colima ssh -- sudo tee /etc/docker/daemon.json.bak > /dev/null
    echo "$UPDATED_DAEMON"  | colima ssh -- sudo tee /etc/docker/daemon.json     > /dev/null
    echo "Restarting Docker (~30s; also bounces k3s)..."
    colima ssh -- sudo systemctl restart docker
    echo "Waiting for k3s to come back..."
    until kubectl get nodes 2>/dev/null | grep -q Ready; do sleep 3; done
fi

# --- Verify K8s is ready ---
echo ""
echo "--- Verifying Kubernetes ---"
kubectl cluster-info 2>/dev/null && echo "K8s cluster is ready." || echo "WARNING: K8s cluster not ready."

# --- Download Go dependencies ---
echo ""
echo "--- Downloading Go dependencies ---"
go mod download

echo ""
echo "=== Setup Complete ==="
echo ""
echo "Next steps:"
echo "  1. tilt up              # Start all services + infra"
echo "  2. Open http://localhost:8080  # Dashboard"
echo "  3. Open http://localhost:8080/dev/publisher-simulator  # See ads"
echo ""
echo "Or run manually:"
echo "  go run ./cmd/simulator single --geo GBR --device mobile"
echo ""
