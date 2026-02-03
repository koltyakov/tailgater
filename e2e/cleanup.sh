#!/bin/bash
# Cleanup E2E test environment

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo "Stopping Docker containers..."
docker-compose down -v 2>/dev/null || true

echo "Cleaning up generated files..."
rm -rf ssh_keys/
rm -rf tmp/
rm -f docker/loggen

echo "Cleanup complete"
