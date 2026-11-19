# Заметки по эксплуатации VOX webOS TV ↔ Яндекс Станция Мини 2

[Текущее состояние стенда, результаты проверки HDMI и известные ограничения](PROJECT_STATUS.md).

Для первого запуска на другой модели используйте [руководство по совместимости](TV_COMPATIBILITY.md#как-проверить-свой-телевизор). Скрипты полного развёртывания и отката ниже рассчитаны на уже подготовленный стенд VOX.

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
1. **Двухрежимный вывод ALSA на Станции (`default` dmix + прямой `hw:0,0`):**
   * **Обычный режим (`hw=false`, YouTube / Spotify / медиаплееры):** обратимый `mount --bind /data/audiosync/asound.lowlat.conf /etc/asound.conf` (`dmix 1920/480`, 40 мс буфер / 10 мс период) с однократным перезапуском `maind --mediad` при монтировании (под присмотром `quasar_launcher2`) для выравнивания IPC + `aplay -D default --buffer-size=1440 --period-size=480 -R 20000`. Сохраняет `amixer Master` (`softvol`) и полную совместимость с Алисой ($L_{\text{st,min}} \approx 45\text{–}50\text{ мс}$).
   * **Режим низкой задержки (`hw=true`, HDMI2 / PS5 Game Mode):** включается автоматически, когда `vox_sender.py` передаёт `FLAG_LOW_LATENCY` (`bit2` в байте 18 `SYN3` для `appId` с префиксом `com.webos.app.hdmi`) ИЛИ когда `ACQUIRE` замеряет $L_{\text{tv}} < 45\text{ мс}$. Использует прямой вывод `aplay -D hw:0,0 --buffer-size=1200 --period-size=240 -R 15000` (25 мс буфер, 5 мс период, 15 мс стартовый порог и минимум очереди 8 мс). Поскольку `hw:0,0` обходит ALSA `softvol`, громкость масштабируется в Go (`SoftVolume`) по той же кривой `-51.0..0.0 dB` с плавным 15 мс сглаживанием без щелчков. Если `hw:0,0` занят Алисой (`Device or resource busy`), `station-rx` автоматически откатывается на `default` (`dmix 1920/480`). При Mute / 10 с тишины / Standby `aplay` мгновенно закрывается (`pcm0p = closed`), освобождая устройство для Алисы.
2. **Захват `hw:0,12` и отслеживание приложений на VOX webOS:**
   * `MARS DSNOOP` (`hw:0,12`) снимает сигнал *pre-volume* и *pre-mute*. Громкость Станции масштабируется по формуле `round(80 * tv / 12)` (`12 → 80%`, `15+ → 100%`), параметры задаются в `/var/lib/webosbrew/vox_sync.conf`.
   * `vox_sender.py` подписан на `luna://com.webos.applicationManager/getForegroundAppInfo '{"subscribe":true}'`: при переключении приложения/входа выставляет `FLAG_LOW_LATENCY` для `com.webos.app.hdmi*` и отправляет `FLAG_SESSION_RESET`, чтобы Станция сразу запускала чистый `ACQUIRE`.
   * Для программного управления пультом на webOS 10.1.3 (например, запуск трека в `spotify-beehive`) работает инъекция Linux `input_event` (`qqHHi`) в `/dev/input/event2` (`LGE M-RCU - Builtin [0]`), а мгновенное глушение срабатывает по `Mute` или `luna://com.webos.service.audio/master/setVolume '{"volume":0}'`.
3. **Исполнитель задержки ТВ (`avSyncSpeaker`):** `TvDelayActuator` и протокол `SAK2`/`TVAP` полностью реализованы, но выключены по умолчанию (`tv_delay_enabled = false`, `/data/audiosync/tv_actuator_enabled = 0`), так как `avSyncSpeaker` работает *pre-tap*.
4. **Бэкап прежней конфигурации Android TV:** сохранён на Рабочем столе в `/Users/leo/Desktop/station-backup-android`.
