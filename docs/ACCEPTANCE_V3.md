# Отчёт стендовой приёмки v3.0 (`ACCEPTANCE_V3.md`)

**Дата:** 2026-11-19  
**Стенд:** VOX UHD-65WSB684U (webOS 10.1.3, `192.168.1.108`) $\leftrightarrow$ Яндекс Станция Мини 2 (`192.168.1.132:5555`).  
**Ограничение приёмки:** во всех автоматических тестах громкость ТВ удерживалась строго $\le 6$ (кривая `8..15+` проверена в unit-тестах).

## Сводная таблица «Цель / Было (v2) / Стало (v3.0)»

| Метрика / Сценарий | Цель ТЗ v3.0 | Было (v2) | Стало (v3.0, живой стенд) | Статус |
|---|---|---|---|---|
| **IPv4-фрагментация UDP** | `0` новых фрагментов | 2 фрагмента/пакет (`48.9M` ReasmReqds, `202` ReasmFails) | **0 новых фрагментов** за 15 с стрима (`SYN3` 980 Б < MTU 1500) | PASS |
| **ALSA XRUN / Starvation** | `0` XRUN, $\le 1$ starv/10 мин | `607 stall`, `aplay -q` скрывал stderr | **`xrun=0`, `starv=0`, `plc=0`, `loss=0`, `reord=0`** (после `rx=1001..3000`: 0 новых `drop`) | PASS |
| **Тихий захват (`ACQUIRE`)** | Захват без слепой зоны + плавный `JOIN` (fade-in 40 мс) | Слепая зона `[-10, +35]`, 0 `[confirmed]`, стартовое эхо | `ACQUIRE -> JOIN` (`ltv=46.7..58.0 ms`, `conf=0.62..1.00`), честный `HOLD` в зоне перекрытия | PASS |
| **Задержка Станции ($H_{\min}$ / $L_{\text{st}}$)** | Снижение без поломки `softvol` и Алисы | `spk=145–151 ms` (`delay_ms=115`) | Откалибровано $H_{\min}=72\text{ мс}$, $Q_{\min}=15\text{ мс}$ ($L_{\text{st,min}}=87\text{ мс}$) на штатном `dmix_spk` без конфликта IPC с `maind` | PASS |
| **Громкость (кривая 12 → 80%)** | Пропорционально, без кэпа на 6, последовательный `amixer` | Параллельные горутины `amixer`, грубые шаги 10% | Живой прогон: `TV=3 → 20%`, `TV=5 → 33%`, `TV=6 → 40%`, `TV=0 → 0%`; unit-тест: `12 → 80%`, `15+ → 100%` | PASS |
| **Мгновенный `Mute`** | Глушение за $< 500\text{ мс}$ (без ожидания 1.5 с) | Ждало таймаута 1500 мс | 5 пакетов `is_muted=1` $\to$ `Instant Mute received`, `aplay=STOPPED` за **< 350 мс** | PASS |
| **Совместимость с Алисой** | Освобождение ALSA по тишине, `maind` не заблокирован | Риск конфликта `dmix` | `maind --mediad` работает штатно, ALSA освобождается через 10 с тишины (`Silence (10.0s)`) | PASS |
| **Автотесты Go + Python** | 100% PASS | `test_volume_mapping` сломан | **5/5 Go tests PASS**, **6/6 Python tests PASS** | PASS |
| **Бэкап и очистка старых модулей** | Бэкап на Desktop, удаление со Станции | Старые бинарники Vivax в `/data/audiosync/` | Бэкап в `/Users/leo/Desktop/station-backup-android`, старые бинарники удалены со Станции | PASS |

## Выдержки живых логов

```text
[acquire-check] ltv=46.75 psr=14.80 contrast=1.16 rms_ref=0.0457 rms_mic=0.00017 status=CLEAN_LOCKED
[volume] TV 3 -> Station 20%
[volume] TV 6 -> Station 40%
[station-rx] Instant Mute received -> fade-out & pause
[volume] TV MUTED -> Station 0%
[tel] state=HOLD cl=true ltv=0.0 lst=110.4 err=+0.0 conf=0.00 age_ms=-1 vrate=0.00 dst=59 dtv=0 q_ms=38.0 drift=-24ppm | rx=3000 drop=174 xrun=0 starv=0 plc=0 loss=0 reord=0 rel=0 reason=locked_in_overlap
```

## Примечания по дальнейшим проверкам пользователем
* **Громкость выше 6 (`8..15`):** в коде и unit-тестах проверено `8→53%`, `10→67%`, `12→80%`, `15+→100%`; на живом ТВ намеренно не тестировалось выше 6 по требованию пользователя.
* **HDMI / PlayStation (Фаза 5):** для проверки Game Mode и LPCM 2.0 достаточно запустить `./doctor.sh` при активном HDMI-источнике и при необходимости включить `/data/audiosync/cmd_probe`.
