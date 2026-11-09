# Архитектура синхронизации звука (TV -> Yandex Station)

Система реализует распределенный аудио-мост реального времени между Android TV (Smart TV на платформе Android 10+) и Яндекс Станцией (Mini 2 / Linux ARM64).

```mermaid
flowchart TD
    subgraph TV["Android TV (Android 10+ / 11)"]
        subgraph Apps["Источники звука"]
            YT["YouTube TV\n(ExoPlayer / Tunneled AV-Sync)"]
            TW["Twitch TV\n(AAudio Native Low-Latency)"]
            OTHER["Другие плееры / Игры"]
        end

        AUD_HAL["Android Audio HAL & AudioFlinger\n(48000 Hz, 16-bit PCM Stereo)"]
        TV_SPK["Встроенные динамики ТВ\n(Задержка видеопроцессора MEMC: 80–210 мс)"]
        
        FOREGROUND["UsageEvents Detector\n(Определение активного пакета)"]
        VOL_LISTENER["VolumeObserver\n(Слушатель громкости STREAM_MUSIC)"]
        
        CAP["TvAudioSync Service\nAudioPlaybackCaptureConfiguration"]
        UDP_TX["UDP Packetizer & Streamer\n(1920 байт PCM + 16 байт header)"]

        Apps --> AUD_HAL
        AUD_HAL --> TV_SPK
        AUD_HAL -.->|Захват без мутирования| CAP
        FOREGROUND -->|Пакет -> целевая задержка| CAP
        VOL_LISTENER -->|Громкость ТВ -> колонка| CAP
        CAP --> UDP_TX
    end

    subgraph NET["Локальная сеть (Wi-Fi 5 GHz / LAN)"]
        UDP_STREAM["UDP Датаграммы (порт 19890)\n[MAGIC 4B][SEQ 4B][TIME 4B][VOL 3B][DELAY 1B][PCM 1920B]"]
    end

    subgraph STATION["Яндекс Станция (Mini 2 / Linux 5.4 aarch64)"]
        UDP_RX["station-rx Daemon\n(Go / Zero-Copy UDP Socket)"]
        VOL_CTL["ALSA Volume Engine\n(amixer Master)"]
        JITTER["Adaptive Jitter & Delay Queue\n(Очередь слайсов 10 мс)"]
        APLAY["aplay Subprocess\n(ALSA raw S16_LE 48000Hz, -B 20000 -R 10000)"]
        WATCHDOG["Silence / Standby Watchdog\n(Освобождение ALSA через 1500 мс)"]
        SPK["Динамик Яндекс Станции"]

        UDP_RX -->|Байты 12-14| VOL_CTL
        UDP_RX -->|Байт 15| JITTER
        UDP_RX --> JITTER
        JITTER --> APLAY
        APLAY --> SPK
        WATCHDOG -.->|Пауза / стоп| APLAY
    end

    UDP_TX --> UDP_STREAM
    UDP_STREAM --> UDP_RX
```

---

## 1. Захват аудио на Android TV (`tv-app`)

- **API захвата**: `AudioPlaybackCaptureConfiguration` (доступен начиная с Android 10).
- **Спецификация аудио**: 48 000 Гц, 16-бит Stereo Linear PCM.
- **Размер кванта (chunk)**: 1920 байт (960 сэмплов = ровно 10 миллисекунд аудио).
- **Zero-touch Permissions**:
  - `PROJECT_MEDIA: allow` устанавливается один раз через `appops`. Android OS не показывает диалог подтверждения `MediaProjectionPermissionActivity`, служба стартует немедленно в фоне.
  - `PACKAGE_USAGE_STATS: allow` даёт доступ к `UsageStatsManager.queryEvents` для чтения событий фокуса окон без прав суперпользователя.

---

## 2. Протокол передачи (UDP Header)

Каждый пакет состоит из 1936 байт: 16 байт заголовок + 1920 байт PCM-данных.

| Смещение | Размер | Поле | Описание |
|---|---|---|---|
| `0..3` | 4 байта | `Magic` | `0x53594E43` (ASCII `"SYNC"`) |
| `4..7` | 4 байта | `Sequence` | 32-битный монотонно возрастающий номер пакета |
| `8..11` | 4 байта | `Timestamp` | Метка времени отправки (`SystemClock.elapsedRealtime() % 2^32`) |
| `12` | 1 байт | `TV Volume` | Текущий уровень громкости ТВ (`0..100`) |
| `13` | 1 байт | `Max TV Volume` | Максимальный уровень громкости ТВ |
| `14` | 1 байт | `Station Target Vol` | Вычисленный уровень для Станции по формуле: `(V_tv + 1) / 2` |
| `15` | 1 байт | `Target Delay / 10` | Требуемая акустическая задержка в единицах по 10 мс (например, `8` = 80 мс, `21` = 210 мс) |
| `16..1935` | 1920 байт | `Audio PCM` | 10 мс несжатого звука S16_LE 48000Hz stereo |

---

## 3. Приемник звука на Яндекс Станции (`station-daemon`)

Написан на Go и скомпилирован статически под Linux ARM64 (`aarch64`).
- **Слушает UDP-порт `19890`**.
- **Динамическая очередь задержки**:
  - Байт 15 парсится на лету. При переключении с YouTube (210 мс) на Twitch (80 мс) демон плавно перестраивает глубину очереди без щелчков.
  - Очередь буферизует ровно $N$ чанков по 10 мс:
    - Для YouTube: $\sim 12$ чанков (120 мс в очереди Go + 90 мс аппаратный путь ALSA = 210 мс).
    - Для Twitch: $\sim 2$ чанка (20 мс в очереди Go + 60 мс аппаратный путь ALSA = 80 мс).
- **ALSA Low-Latency параметры**:
  - Вызов `aplay -D default -B 20000 -R 10000 -t raw -f S16_LE -r 48000 -c 2 -q -`.
  - Уменьшение аппаратного буфера драйвера ALSA с 40 мс до 20 мс с порогом старта 10 мс исключает внутреннюю задержку ядра Linux.
- **Освобождение аудиоустройства (Standby Watchdog)**:
  - При отсутствии UDP-пакетов более 1500 мс демон закрывает stdin `aplay` и процесс завершается.
  - Это освобождает ALSA-устройство для штатного голосового ассистента Алиса и фоновых сервисов Яндекс Музыки (Quasar).
