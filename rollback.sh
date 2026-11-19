#!/bin/bash
set -euo pipefail

export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
BACKUP_DIR="${BACKUP_DIR:-/Users/leo/Desktop/station-backup-android}"
STATION_ADDR="${STATION_ADDR:-192.168.1.132:5555}"
TV_HOST="${TV_HOST:-root@192.168.1.108}"
TV_KEY="${TV_KEY:-/Users/leo/Documents/Codex/2026-11-18/webos-adb-lg-webos-lg-tv/outputs/vox-access/vox_root_ed25519}"
SSH_OPTS="-F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i $TV_KEY"

echo "=== 1. Rollback Station ALSA mount & restore backup ==="
adb connect "$STATION_ADDR" >/dev/null
adb -s "$STATION_ADDR" shell '
/etc/init.d/S99station-rx stop 2>/dev/null || true
umount /etc/asound.conf 2>/dev/null || true
'
if [ -f "$BACKUP_DIR/audiosync/station-rx" ]; then
  adb -s "$STATION_ADDR" push "$BACKUP_DIR/audiosync/station-rx" /data/audiosync/station-rx
  adb -s "$STATION_ADDR" push "$BACKUP_DIR/S99station-rx" /etc/init.d/S99station-rx
  adb -s "$STATION_ADDR" shell 'chmod +x /data/audiosync/station-rx /etc/init.d/S99station-rx && /etc/init.d/S99station-rx start'
  echo "[OK] Station restored from $BACKUP_DIR"
else
  echo "[WARN] Backup not found at $BACKUP_DIR; ALSA mount reverted and daemon stopped."
fi

echo "=== 2. Restore TV avSyncSpeaker to 0 and restart sender ==="
ssh $SSH_OPTS "$TV_HOST" '
luna-send -n 1 luna://com.webos.settingsservice/setSystemSettings "{\"category\":\"sound\",\"settings\":{\"avSyncSpeaker\":\"0\"}}" >/dev/null 2>&1 || true
rm -f /tmp/vox_orig_tv_delay
/var/lib/webosbrew/init.d/80-vox-audio-sync restart 2>/dev/null || true
'
echo "=== Rollback Complete ==="
