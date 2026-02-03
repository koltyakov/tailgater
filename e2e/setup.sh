#!/bin/bash
# E2E Test Setup Script

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo "=== Tailgater E2E Test Setup ==="

# Generate SSH keys if they don't exist
if [ ! -f ssh_keys/id_rsa ]; then
    echo "Generating SSH keys..."
    mkdir -p ssh_keys
    ssh-keygen -t rsa -b 2048 -f ssh_keys/id_rsa -N "" -C "tailgater-e2e"
    cp ssh_keys/id_rsa.pub ssh_keys/authorized_keys
    chmod 600 ssh_keys/authorized_keys
    echo "SSH keys generated in ssh_keys/"
else
    echo "SSH keys already exist"
fi

# Build log generator for Linux
echo "Building log generator for Linux..."
cd loggen
GOOS=linux GOARCH=amd64 go build -o ../docker/loggen .
cd ..

# Copy files to tmp/ for Docker build context
echo "Preparing build context..."
mkdir -p tmp
cp docker/start.sh tmp/
cp docker/loggen tmp/loggen-binary
cp ssh_keys/authorized_keys tmp/

# Build and start containers
echo "Building and starting Docker containers..."
docker-compose down -v 2>/dev/null || true
docker-compose -f docker-compose.yml up --build -d

# Wait for containers to be ready
echo "Waiting for containers to start..."
sleep 3

# Test SSH connectivity
echo "Testing SSH connectivity..."
for port in 2221 2222 2223; do
    if ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
           -o ConnectTimeout=5 -p $port -i ssh_keys/id_rsa testuser@localhost "echo OK" 2>/dev/null; then
        echo "  server on port $port: OK"
    else
        echo "  server on port $port: FAILED (will retry)"
    fi
done

echo ""
echo "=== Setup Complete ==="
echo ""
echo "Run tailgater with:"
echo "  ../tailgater -config tailgater.e2e.yaml"
echo ""
echo "Or for web mode:"
echo "  ../tailgater -config tailgater.e2e.yaml -web"
echo ""
echo "Dashboard will be at: http://localhost:8888"
echo ""
echo "To stop test containers:"
echo "  docker-compose down"
