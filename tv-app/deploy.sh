#!/bin/bash
set -e

export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"

if [ -z "$1" ]; then
    echo "Использование: ./deploy.sh <IP_ТЕЛЕВИЗОРА> [ADB_PORT]"
    echo "Пример: ./deploy.sh 192.168.1.68"
    exit 1
fi

TV_IP="$1"
ADB_PORT="${2:-5555}"

echo "=== Развертывание TvAudioSync на Android TV ($TV_IP) через ADB (порт $ADB_PORT) ==="

DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

./build.sh

if [ "$ADB_PORT" != "5555" ]; then
    ADB="adb -P $ADB_PORT"
else
    ADB="adb"
fi

echo "Подключение к $TV_IP:5555..."
$ADB connect "$TV_IP:5555" || true

echo "Установка APK..."
$ADB -s "$TV_IP:5555" shell "rm -rf /data/data/com.spotify.tv.android/cache/* /data/local/tmp/*.tar 2>/dev/null; pm trim-caches 1000M 2>/dev/null || true"
$ADB -s "$TV_IP:5555" install --no-incremental -r -g TvAudioSync.apk

echo "Выдача фоновых разрешений (AppOps)..."
$ADB -s "$TV_IP:5555" shell appops set dev.antigravity.audiosync PROJECT_MEDIA allow
$ADB -s "$TV_IP:5555" shell appops set dev.antigravity.audiosync GET_USAGE_STATS allow 2>/dev/null || true
$ADB -s "$TV_IP:5555" shell appops set dev.antigravity.audiosync PACKAGE_USAGE_STATS allow 2>/dev/null || true
$ADB -s "$TV_IP:5555" shell appops set dev.antigravity.audiosync SYSTEM_ALERT_WINDOW allow
$ADB -s "$TV_IP:5555" shell pm grant dev.antigravity.audiosync android.permission.WRITE_SECURE_SETTINGS 2>/dev/null || true

echo "Запуск службы..."
$ADB -s "$TV_IP:5555" shell am start -n dev.antigravity.audiosync/.MainActivity --ez EXTRA_AUTO_START true --es EXTRA_IP 192.168.1.132

echo "=== Установка на TV успешно завершена! ==="
