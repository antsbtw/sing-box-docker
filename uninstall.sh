#!/bin/bash
set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

INSTALL_DIR="/opt/otun-agent"

echo -e "${YELLOW}This will remove OTun Node Agent and all its data.${NC}"
read -p "Are you sure? (y/N) " -n 1 -r
echo

if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Cancelled."
    exit 0
fi

echo -e "${GREEN}[INFO]${NC} Stopping services..."
cd "$INSTALL_DIR" 2>/dev/null && docker compose down || true

echo -e "${GREEN}[INFO]${NC} Removing files..."
rm -rf "$INSTALL_DIR"
rm -f /usr/local/bin/otun

echo -e "${GREEN}[INFO]${NC} OTun Node Agent has been removed."
