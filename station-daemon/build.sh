#!/bin/bash
set -e

DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

echo "=== Building Station Audio Daemon for Linux ARM64 (Yandex Station Mini 2 / Allwinner H616) ==="

export CGO_ENABLED=0
export GOOS=linux
export GOARCH=arm64

go build -ldflags="-s -w" -o station-rx main.go

echo "=== Successfully built: station-rx ==="
ls -lh station-rx
