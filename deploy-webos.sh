#!/bin/bash
set -euo pipefail

export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
STATION_ADDR="${STATION_ADDR:-192.168.1.132:5555}"
TV_HOST="${TV_HOST:-root@192.168.1.108}"
TV_KEY="${TV_KEY:-/Users/leo/Documents/Codex/2026-11-18/webos-adb-lg-webos-lg-tv/outputs/vox-access/vox_root_ed25519}"
SSH_OPTS="-F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i $TV_KEY"

echo "=== 1. Run Unit Tests & Build ARMv7 station-rx ==="
(cd "$REPO_DIR/station-daemon" && go test -v ./... && CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o station-rx .)
(cd "$REPO_DIR/webos-sender" && python3 -m unittest -v test_vox_sender.py)

echo "=== 2. Deploy Station Daemon & Low-Latency ALSA Profile (mount --bind) ==="
adb connect "$STATION_ADDR" >/dev/null
adb -s "$STATION_ADDR" shell '/etc/init.d/S99station-rx stop 2>/dev/null || true; umount /etc/asound.conf 2>/dev/null || true'
adb -s "$STATION_ADDR" push "$REPO_DIR/station-daemon/station-rx" /data/audiosync/station-rx
adb -s "$STATION_ADDR" push "$REPO_DIR/station-daemon/asound.lowlat.conf" /data/audiosync/asound.lowlat.conf
adb -s "$STATION_ADDR" shell '
chmod +x /data/audiosync/station-rx
echo "closed_loop" > /data/audiosync/auto_control
echo "0" > /data/audiosync/tv_actuator_enabled
echo "56" > /data/audiosync/delay_ms
/etc/init.d/S99station-rx start
'

echo "=== 3. Deploy webOS Sender v3.0 ==="
ssh $SSH_OPTS "$TV_HOST" '/var/lib/webosbrew/init.d/80-vox-audio-sync stop 2>/dev/null || true'
scp $SSH_OPTS "$REPO_DIR/webos-sender/vox_sender.py" "$TV_HOST:/var/lib/webosbrew/vox_sender.py"
scp $SSH_OPTS "$REPO_DIR/webos-sender/vox_sync.conf" "$TV_HOST:/var/lib/webosbrew/vox_sync.conf"
scp $SSH_OPTS "$REPO_DIR/webos-sender/80-vox-audio-sync" "$TV_HOST:/var/lib/webosbrew/init.d/80-vox-audio-sync"
ssh $SSH_OPTS "$TV_HOST" 'chmod +x /var/lib/webosbrew/vox_sender.py /var/lib/webosbrew/init.d/80-vox-audio-sync && /var/lib/webosbrew/init.d/80-vox-audio-sync start'

echo "=== Deploy v3.0 Complete ==="
