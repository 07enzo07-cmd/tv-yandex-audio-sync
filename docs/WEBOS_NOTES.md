# Заметки по эксплуатации VOX webOS TV ↔ Яндекс Станция Мини 2 (v3.0)

## Быстрые команды (из корня репозитория)
* **Деплой (тесты + сборка ARMv7 + Станция + ТВ):** `./deploy-webos.sh`
* **Диагностика одной командой:** `./doctor.sh`
* **Полный откат на бэкап:** `./rollback.sh`
* **Ручной чистый замер `probe` на Станции (без слепой зоны):**
  ```bash
  adb -s 192.168.1.132:5555 shell 'echo 2000 > /data/audiosync/cmd_probe'
  adb -s 192.168.1.132:5555 shell 'grep "\[probe\]" /tmp/station-rx.log | tail -n 5'
  ```

## Важные аппаратные особенности стенда
1. **ALSA `dmix_spk` на Станции:** `maind --mediad` (Алиса) держит `dmix_spk` (`ipc_key 1024`) открытым с `buffer_size=7680, period_size=960`. Менять `dmix_spk` на лету через `mount --bind /etc/asound.conf` при работающем `maind` нельзя — это переводит `pcm0p` в заклинившее состояние `PREPARED`. Поэтому `station-rx` работает поверх штатного `/etc/asound.conf` с `aplay --buffer-size=2880 --period-size=960 -R 20000` ($H_{\min} \approx 72\text{ мс}$, $L_{\text{st,min}} = 87\text{ мс}$), сохраняя `softvol` (`amixer Master`) и полную совместимость с Алисой.
2. **Захват `hw:0,12` на VOX webOS:** `MARS DSNOOP` снимает сигнал *pre-volume* и *pre-mute*. Громкость Станции масштабируется независимо по формуле `round(80 * tv / 12)` (`12 → 80%`, `15+ → 100%`), параметры задаются в `/var/lib/webosbrew/vox_sync.conf`.
3. **Исполнитель задержки ТВ (`avSyncSpeaker`):** `TvDelayActuator` и протокол `SAK2`/`TVAP` полностью реализованы, но выключены по умолчанию (`tv_delay_enabled = false`, `/data/audiosync/tv_actuator_enabled = 0`), так как Фаза 0.3 не подтвердила *post-tap* сдвиг на встроенном плеере VOX.
4. **Бэкап прежней конфигурации Android TV:** сохранён на Рабочем столе в `/Users/leo/Desktop/station-backup-android`.
