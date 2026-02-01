#!/bin/bash
set -e

# Script to run tests in a Linux container for debugging platform-specific issues

IMAGE_NAME="learnersync-test"

echo "Building Docker image..."
docker build -f Dockerfile.test -t $IMAGE_NAME .

echo ""
echo "Running all tests in Linux container..."
docker run --rm -v "$(pwd):/app" $IMAGE_NAME

echo ""
echo "Tests completed!"
