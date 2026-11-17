#!/bin/bash
set -e

DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

echo "=== Building Station Audio Daemon for Linux ARMv7 (Yandex Station Mini 2) ==="

export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
export CGO_ENABLED=0
export GOOS=linux
export GOARCH=arm
export GOARM=7

go build -ldflags="-s -w" -o station-rx .

echo "=== Successfully built: station-rx ==="
ls -lh station-rx
