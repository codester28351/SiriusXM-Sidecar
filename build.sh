#!/usr/bin/env bash
set -e
go test ./...
go build -trimpath -o siriusxm-sidecar ./cmd/siriusxm-sidecar
echo "Built siriusxm-sidecar"
