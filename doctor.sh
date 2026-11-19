#!/bin/bash
set -euo pipefail

export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
STATION_ADDR="${STATION_ADDR:-192.168.1.132:5555}"
TV_HOST="${TV_HOST:-root@192.168.1.108}"
TV_KEY="${TV_KEY:-/Users/leo/Documents/Codex/2026-11-18/webos-adb-lg-webos-lg-tv/outputs/vox-access/vox_root_ed25519}"
SSH_OPTS="-F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i $TV_KEY"

echo "================ STATION DOCTOR ($STATION_ADDR) ================"
adb connect "$STATION_ADDR" >/dev/null
adb -s "$STATION_ADDR" shell '
echo "SHA256: $(sha256sum /data/audiosync/station-rx 2>/dev/null)"
echo "auto_control=$(cat /data/audiosync/auto_control 2>/dev/null) delay_ms=$(cat /data/audiosync/delay_ms 2>/dev/null) tv_actuator=$(cat /data/audiosync/tv_actuator_enabled 2>/dev/null)"
echo "asound.conf mount: $(grep " /etc/asound.conf " /proc/mounts 2>/dev/null || echo none)"
echo "asound.conf diff vs bak:"
diff -u /data/audiosync/asound.conf.bak /etc/asound.conf 2>/dev/null | head -n 15 || true
echo "Processes:"
ps | grep -E "station-rx|aplay" | grep -v grep || true
echo "Recent [tel] / [acquire] / [track]:"
grep -E "\[tel\]|\[acquire\]|\[track\]|\[alsa-xrun\]" /tmp/station-rx.log 2>/dev/null | tail -n 12 || true
'

echo "================ WEBOS TV DOCTOR ($TV_HOST) ================"
ssh $SSH_OPTS "$TV_HOST" '
echo "SHA256: $(sha256sum /var/lib/webosbrew/vox_sender.py 2>/dev/null)"
/var/lib/webosbrew/init.d/80-vox-audio-sync status || true
echo "Luna sound:"
luna-send -n 1 luna://com.webos.settingsservice/getSystemSettings "{\"category\":\"sound\",\"keys\":[\"avSyncSpeaker\",\"avSync\",\"soundOutput\"]}" 2>/dev/null
luna-send -n 1 luna://com.webos.service.audio/master/getVolume "{}" 2>/dev/null
echo "Recent vox_sender.log:"
tail -n 10 /tmp/vox_sender.log 2>/dev/null || true
'
