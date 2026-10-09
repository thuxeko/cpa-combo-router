#!/bin/sh
# Build + test the combo-router plugin inside a throwaway Go container.
#
# The host has no Go toolchain, so the plugin (a cgo c-shared object) is built
# in the official golang image. Module cache lives in a named volume so repeat
# builds do not re-download CLIProxyAPI.
#
# Usage:
#   sh build-plugin.sh test    # go test ./...
#   sh build-plugin.sh build   # go build -> dist/combo-router.so
#   sh build-plugin.sh check   # gofmt check + vet + test
set -e

SRC=/mnt/dungchung/cpa-combo-router
IMAGE=golang:1.26

# The Go module cache is shared across runs; the build cache is not, so a clean
# clone never reuses stale objects.
docker run --rm \
  -v "$SRC":/src \
  -v combo-router-gomod:/go/pkg/mod \
  -v combo-router-gocache:/root/.cache/go-build \
  -w /src \
  "$IMAGE" \
  sh -c "$1"
