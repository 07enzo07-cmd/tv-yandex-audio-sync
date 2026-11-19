# Отчёт стендовой приёмки v3.2 (`ACCEPTANCE_V3.md`)

**Дата:** 2026-11-19  
**Стенд:** VOX UHD-65WSB684U (webOS 10.1.3, `192.168.1.108`) $\leftrightarrow$ Яндекс Станция Мини 2 (`192.168.1.132:5555`).  
**Ограничение приёмки:** во всех автоматических тестах громкость ТВ не превышала `7` (кривая `8..15+` проверена в unit-тестах), после тестов возвращена на `5`.

## Сводная таблица «Цель / Было (v2) / Стало (v3.2)»

| Метрика / Сценарий | Цель ТЗ v3.2 | Было (v2) | Стало (v3.2, живой стенд) | Статус |
|---|---|---|---|---|
| **Точность синхронизации (YouTube)** | Медиана $|e| \le 4\text{ мс}$ (без эха) | `spk=145–151 мс` при $L_{\text{tv}} \approx 55\text{–}60\text{ мс}$ ($e \approx -90\text{ мс}$, сильное эхо) | **`ltv=60.3 мс`, `lst=61.9 мс` $\Rightarrow e = -1.6\text{ мс}$** (`dmix 1920/480` + `aplay 1440/480`, `hw=false`, `drop=0, xrun=+0, starv=+0, loss=0` на >30 000 пакетов) | PASS |
| **Spotify (`spotify-beehive`)** | Живой трек (`RMS > 100`), автосинхронизация без эха | Не проверялся / на паузе | Запуск трека через `/dev/input/event2` (`RMS=2351..12993`), **`cmd_probe`: `ltv=52.22 мс, psr=13.85, CLEAN_LOCKED`**, `dmix 1920/480` (`hw=false`), **`drop=0, xrun=+0, starv=+0, loss=0`** | PASS |
| **HDMI2 / PS5 Game Mode (`com.webos.app.hdmi2`)** | Автопереключение на прямой `hw:0,0`, программная громкость, покрытие $L_{\text{tv}} \approx 21.5\text{ мс}$ | Пол `dmix` ~45–50 мс $\Rightarrow$ эхо ~25–35 мс | Автоматический флаг `FLAG_LOW_LATENCY` (`bit2` в `SYN3`) по `getForegroundAppInfo` + автопорог `L_tv < 45 мс`; прямой вывод **`aplay -D hw:0,0 --buffer-size=720 --period-size=240 -R 10000`** (`Q_min=8 мс`, $L_{\text{st,min}} \approx 21\text{ мс}$); программная громкость `SoftVolume` (`-51..0 dB`, сглаживание 15 мс) и автоматический fallback на `default` при занятости `hw:0,0` | PASS |
| **Сквозные переключения (без рестарта демонов)** | YouTube $\to$ Spotify $\to$ HDMI2 $\to$ YouTube | Требовался ручной рестарт / профили | На каждом переходе `vox_sender.py` шлёт `session_reset`, Станция делает чистый `ACQUIRE` (с паузой 850 мс на переключение аудиотракта webOS) и сама переключает `default (1920/480)` $\leftrightarrow$ `hw:0,0 (720/240)` | PASS |
| **IPv4-фрагментация UDP** | `0` новых фрагментов | 2 фрагмента/пакет (`48.9M` ReasmReqds, `202` ReasmFails) | **0 новых фрагментов** (`SYN3` 5 мс / 980 Б < MTU 1500) | PASS |
| **Громкость (кривая 12 → 80%)** | Пропорционально, без кэпа на 6 | Параллельные горутины `amixer` | Живой прогон ($\le 7$): `0→0%`, `5→33%`, `6→40%`, `7→47%` (`-27.00 dB`); unit-тест: `12→80%`, `15+→100%` + `SoftVolume` в режиме `hw:0,0` | PASS |
| **Мгновенный `Mute` и Алиса** | Глушение $< 500\text{ мс}$, освобождение `pcm0p=closed` | Ожидание таймаута 1500 мс | `5× is_muted=1` $\to$ `aplay=STOPPED` за **< 350 мс**, `pcm0p=closed`, освобождение через 10 с тишины; `maind --mediad` и `quasar_launcher2` работают штатно | PASS |
| **Автотесты Go + Python** | 100% PASS | 5 Go / 6 Python | **7/7 Go** + **7/7 Python** PASS | PASS |

## Аппаратные замеры `hw:0,0` на Станции (v3.2)
При `pcm0p=closed` проверены три профиля прямого вывода `aplay -D hw:0,0` (`48000 Hz, S16_LE, stereo`):
* `360/120` (7.5 мс буфер / 2.5 мс период): `delay=296` (6.17 мс), 0 XRUN на локальном потоке.
* `480/120` (10.0 мс буфер / 2.5 мс период): `delay=480` (10.0 мс), 0 XRUN на локальном потоке, но единичные XRUN 0.5–4.7 мс под Wi-Fi джиттером.
* `720/240 -R 10000` (15.0 мс буфер / 5.0 мс период = ровно 3 кадра `SYN3`): `delay=656` (13.67 мс), **выбран как основной профиль `hw:0,0`** ($H_{\min} \approx 13\text{ мс}$, $Q_{\min} = 8\text{ мс}$, $L_{\text{st,min}} \approx 21\text{ мс}$).

## Выдержки живой телеметрии v3.2
```text
# Spotify (vol=7, hw=false, dmix 1920/480):
[probe] ltv=52.22 psr=13.85 contrast=1.12 peak_idx=836 rms_ref=0.16758 rms_mic=0.00020 status=CLEAN_LOCKED

# Переключение Spotify -> HDMI2 (PS5) -> YouTube (без перезапуска демонов):
[+] Foreground app changed: spotify-beehive -> com.webos.app.hdmi2 (low_latency=True) -> session_reset
[station-rx] aplay started (pid 30376, hw=true) state=TRACK dst=71 ms targetQ=46.3 ms
[+] Foreground app changed: com.webos.app.hdmi2 -> youtube.leanback.v4 (low_latency=False) -> session_reset
[station-rx] aplay started (pid 30403, hw=false) state=TRACK dst=71 ms targetQ=46.0 ms
```
