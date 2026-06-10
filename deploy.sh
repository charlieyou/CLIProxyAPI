#!/usr/bin/env bash
#
# deploy.sh - Build and deploy CLIProxyAPI to the systemd service
#
set -euo pipefail

SRC_DIR="$(cd "$(dirname "$0")" && pwd)"
DEPLOY_DIR="/opt/cliproxyapi"
SERVICE_NAME="cliproxyapi.service"
BINARY_NAME="cli-proxy-api"
MODULE_PATH="github.com/router-for-me/CLIProxyAPI/v7/cmd/server"
LDFLAGS_PKG="main"

# Get version info from git
VERSION="$(git -C "$SRC_DIR" describe --tags --always --dirty 2>/dev/null || echo "dev")"
COMMIT="$(git -C "$SRC_DIR" rev-parse --short HEAD 2>/dev/null || echo "none")"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

echo "Building ${BINARY_NAME}..."
echo "  Version:    ${VERSION}"
echo "  Commit:     ${COMMIT}"
echo "  Build Date: ${BUILD_DATE}"

LDFLAGS="-X ${LDFLAGS_PKG}.Version=${VERSION} -X ${LDFLAGS_PKG}.Commit=${COMMIT} -X ${LDFLAGS_PKG}.BuildDate=${BUILD_DATE}"

go build -ldflags "$LDFLAGS" -o "${SRC_DIR}/${BINARY_NAME}" "./${MODULE_PATH#*/v7/}"

echo "Build succeeded."

# Stop service before copying (avoids "Text file busy")
echo "Stopping ${SERVICE_NAME}..."
sudo systemctl stop "$SERVICE_NAME"

# Deploy
mkdir -p "$DEPLOY_DIR"
cp "${SRC_DIR}/${BINARY_NAME}" "${DEPLOY_DIR}/${BINARY_NAME}"
cp "${SRC_DIR}/config.yaml" "${DEPLOY_DIR}/config.yaml"

echo "Deployed to ${DEPLOY_DIR}"

# Start service
echo "Starting ${SERVICE_NAME}..."
sudo systemctl start "$SERVICE_NAME"
sleep 2
systemctl status "$SERVICE_NAME" --no-pager

echo "Done."
