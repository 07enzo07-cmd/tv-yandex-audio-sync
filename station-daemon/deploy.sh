#!/bin/bash
set -e

export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"

if [ -z "$1" ]; then
    echo "Использование: ./deploy.sh <IP_СТАНЦИИ> [ADB_PORT]"
    echo "Пример: ./deploy.sh 192.168.1.132"
    exit 1
fi

STATION_IP="$1"
ADB_PORT="${2:-5555}"

echo "=== Развертывание демона на Яндекс Станцию ($STATION_IP) через ADB (порт $ADB_PORT) ==="

DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

./build.sh

if [ "$ADB_PORT" != "5555" ]; then
    ADB="adb -P $ADB_PORT"
else
    ADB="adb"
fi

echo "Подключение к $STATION_IP:5555..."
$ADB connect "$STATION_IP:5555" || true

echo "Копирование бинарника..."
$ADB -s "$STATION_IP:5555" shell mkdir -p /data/audiosync
device="$STATION_IP:5555"
$ADB -s "$device" push station-rx /data/audiosync/station-rx.new
$ADB -s "$device" shell 'set -e
cd /data/audiosync
chmod 755 station-rx.new
mv -f station-rx.new station-rx
old=$(pidof station-rx || true)
if [ -n "$old" ]; then
    kill -TERM $old
    for i in 1 2 3 4 5; do
        pidof station-rx >/dev/null || break
        sleep 1
    done
    if pidof station-rx >/dev/null; then exit 1; fi
fi
/etc/init.d/S99station-rx start
sleep 1
pidof station-rx'
