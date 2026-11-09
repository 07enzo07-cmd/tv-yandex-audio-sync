#!/bin/bash
set -e

if [ -z "$1" ]; then
    echo "Использование: ./deploy.sh <IP_СТАНЦИИ> [ADB_PORT]"
    echo "Пример: ./deploy.sh 192.168.1.100"
    exit 1
fi

STATION_IP="$1"
ADB_PORT="${2:-5555}"

echo "=== Развертывание демона на Яндекс Станцию ($STATION_IP) через ADB (порт $ADB_PORT) ==="

DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

if [ ! -f "station-rx" ]; then
    echo "Бинарник station-rx не найден, запускаем сборку..."
    ./build.sh
fi

if [ "$ADB_PORT" != "5555" ]; then
    ADB="adb -P $ADB_PORT"
else
    ADB="adb"
fi

echo "Подключение к $STATION_IP:5555..."
$ADB connect "$STATION_IP:5555" || true

echo "Копирование бинарника..."
$ADB -s "$STATION_IP:5555" shell mkdir -p /data/audiosync
$ADB -s "$STATION_IP:5555" push station-rx /data/audiosync/station-rx
$ADB -s "$STATION_IP:5555" shell chmod +x /data/audiosync/station-rx

echo "Перезапуск демона на Станции..."
$ADB -s "$STATION_IP:5555" shell "killall -9 station-rx 2>/dev/null || true"
$ADB -s "$STATION_IP:5555" shell "start-stop-daemon -S -b -x /data/audiosync/station-rx"

echo "=== Развертывание на Станцию успешно завершено! ==="
$ADB -s "$STATION_IP:5555" shell "ps | grep station-rx"
