#!/bin/bash
set -e

if [ -z "$1" ]; then
    echo "Использование: ./deploy.sh <IP_ТЕЛЕВИЗОРА> [ADB_PORT]"
    echo "Пример: ./deploy.sh 192.168.1.50"
    exit 1
fi

TV_IP="$1"
ADB_PORT="${2:-5555}"

echo "=== Развертывание TvAudioSync на Android TV ($TV_IP) через ADB (порт $ADB_PORT) ==="

DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

if [ ! -f "TvAudioSync.apk" ]; then
    echo "APK не найден, запускаем сборку..."
    ./build.sh
fi

if [ "$ADB_PORT" != "5555" ]; then
    ADB="adb -P $ADB_PORT"
else
    ADB="adb"
fi

echo "Подключение к $TV_IP:5555..."
$ADB connect "$TV_IP:5555" || true

echo "Установка APK..."
$ADB -s "$TV_IP:5555" install -r -g TvAudioSync.apk

echo "Выдача фоновых разрешений (AppOps)..."
$ADB -s "$TV_IP:5555" shell appops set dev.antigravity.audiosync PROJECT_MEDIA allow
$ADB -s "$TV_IP:5555" shell appops set dev.antigravity.audiosync GET_USAGE_STATS allow 2>/dev/null || true
$ADB -s "$TV_IP:5555" shell appops set dev.antigravity.audiosync PACKAGE_USAGE_STATS allow 2>/dev/null || true
$ADB -s "$TV_IP:5555" shell appops set dev.antigravity.audiosync SYSTEM_ALERT_WINDOW allow

echo "Запуск службы..."
$ADB -s "$TV_IP:5555" shell am start -n dev.antigravity.audiosync/.MainActivity --ez EXTRA_AUTO_START true

echo "=== Установка на TV успешно завершена! ==="
