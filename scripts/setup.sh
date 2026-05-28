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
