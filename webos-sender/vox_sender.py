#!/usr/bin/env python3
"""
VOX webOS to Yandex Station Mini 2 Audio Sender (Protocol v3.0 - SYN3/SAK2/TVAP)
Production Daemon with:
- 5.0 ms PCM framing (960 B PCM + 20 B SYN3 header = 980 B UDP payload, zero IPv4 fragmentation)
- Exact percentage volume mapping (TV=12 -> 80% / level 8, TV=15+ -> 100%, no cap at 6)
- Instant Mute (sends 5x is_muted=1 control packets before pausing TX)
- Smart silence gating with working wake_rms/silence_rms hysteresis (releases Station ALSA for Alice)
- SAK2 & legacy SACK (unsigned confidence!) reception + TvDelayActuator with TVAP ack & auto-rollback
- Low-jitter optimizations: IP_TOS DSCP EF (0xB8), gc.disable() in hot loop, arecord watchdog
"""

import argparse
import gc
import json
import os
import select
import signal
import socket
import struct
import subprocess
import sys
import threading
import time

MAGIC_SYN3 = 0x53594E33  # 'SYN3' (20 B header + 960 B PCM = 980 B)
MAGIC_SYNC = 0x53594E43  # 'SYNC' (legacy v2)
MAGIC_SAK2 = 0x53414B32  # 'SAK2' (18 B telemetry from Station)
MAGIC_SACK = 0x5341434B  # 'SACK' (12 B legacy telemetry)
MAGIC_TVAP = 0x54564150  # 'TVAP' (8 B TV delay apply ack -> Station)

SYN3_HEADER_LEN = 20
CHUNK_FRAMES_48K = 240              # 5.0 ms @ 48 kHz
CHUNK_PCM_LEN = CHUNK_FRAMES_48K * 4  # 960 bytes
PACKET_TOTAL_LEN = SYN3_HEADER_LEN + CHUNK_PCM_LEN  # 980 bytes (< 1500 MTU, zero IPv4 frag!)

FLAG_MUTED = 1 << 0
FLAG_SESSION_RESET = 1 << 1
FLAG_LOW_LATENCY = 1 << 2

TVAP_STATUS_OK = 0
TVAP_STATUS_CLAMPED = 1
TVAP_STATUS_UNSUPPORTED = 2
TVAP_STATUS_ERROR = 3

STATE_NAMES = {
    0: "IDLE",
    1: "ACQUIRE",
    2: "JOIN",
    3: "TRACK",
    4: "REACQUIRE",
    5: "HOLD",
    6: "UNREACHABLE",
}

DEFAULT_STATION_IP = "192.168.1.132"
DEFAULT_STATION_PORT = 12345
DEFAULT_ALSA_DEVICE = "hw:0,12"
DEFAULT_SILENCE_RMS = 20
DEFAULT_WAKE_RMS = 25
DEFAULT_SILENCE_TIMEOUT = 10.0
DEFAULT_CONF_PATH = "/var/lib/webosbrew/vox_sync.conf"
ORIG_TV_DELAY_FILE = "/tmp/vox_orig_tv_delay"

try:
    import audioop

    def calc_rms(chunk):
        if not chunk:
            return 0
        return audioop.rms(chunk, 2)
except ImportError:
    def calc_rms(chunk):
        if not chunk:
            return 0
        frames = len(chunk) // 4
        if frames == 0:
            return 0
        # Sample evenly across stereo frames (preserving L/R pairs, no channel skew)
        step = max(1, frames // 32)
        sum_sq = 0
        count = 0
        for f in range(0, frames, step):
            l, r = struct.unpack_from("<hh", chunk, f * 4)
            sum_sq += l * l + r * r
            count += 2
        return int((sum_sq / max(1, count)) ** 0.5)


def load_config(path=DEFAULT_CONF_PATH):
    cfg = {
        "vol_slope": 80.0 / 12.0,
        "vol_cap": 100,
        "vol_gamma": 1.0,
        "tv_delay_enabled": False,
        "tv_delay_max_ms": 50,
    }
    if not path or not os.path.exists(path):
        return cfg
    try:
        with open(path, "r") as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                k, v = [p.strip() for p in line.split("=", 1)]
                if k == "vol_slope":
                    cfg[k] = float(v)
                elif k in ("vol_cap", "tv_delay_max_ms"):
                    cfg[k] = int(v)
                elif k == "vol_gamma":
                    cfg[k] = float(v)
                elif k == "tv_delay_enabled":
                    cfg[k] = v.lower() in ("1", "true", "yes")
    except Exception:
        pass
    return cfg


def calc_station_volume_pct(tv_vol, is_muted, slope=80.0 / 12.0, cap=100, gamma=1.0):
    """Map TV volume proportionally: TV=12 -> 80% (level 8), TV=15+ -> 100%, no cap at 6."""
    if is_muted or tv_vol <= 0:
        return 0
    if gamma == 1.0:
        raw = tv_vol * slope
    else:
        raw = 100.0 * ((min(1.0, (tv_vol * slope) / 100.0)) ** gamma)
    pct = int(round(raw))
    return max(1, min(int(cap), pct))


class TvDelayActuator:
    """Encapsulates webOS avSyncSpeaker get/set/restore with guaranteed rollback."""

    def __init__(self, enabled=False, max_ms=50, orig_file=ORIG_TV_DELAY_FILE, runner=None):
        self.enabled = enabled
        self.max_ms = max_ms
        self.orig_file = orig_file
        self.runner = runner or self._default_runner
        self.orig_delay_ms = None
        self.current_delay_ms = 0
        self.last_applied_by_us = None
        self.lock = threading.Lock()

        if self.enabled:
            self._init_orig()

    def _default_runner(self, args):
        res = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, timeout=1.5)
        return res.returncode, res.stdout.strip()

    def _init_orig(self):
        # Recover original value if previous run crashed before restoring
        if os.path.exists(self.orig_file):
            try:
                with open(self.orig_file, "r") as f:
                    self.orig_delay_ms = int(f.read().strip())
                self.restore()
                return
            except Exception:
                pass
        val = self.get()
        if val is not None:
            self.orig_delay_ms = val
            self.current_delay_ms = val
            try:
                with open(self.orig_file, "w") as f:
                    f.write(str(val) + "\n")
            except Exception:
                pass

    def get(self):
        try:
            rc, out = self.runner([
                "/usr/bin/luna-send", "-n", "1",
                "luna://com.webos.settingsservice/getSystemSettings",
                '{"category":"sound","keys":["avSyncSpeaker"]}'
            ])
            if rc == 0 and out:
                data = json.loads(out)
                val = data.get("settings", {}).get("avSyncSpeaker")
                if val is not None:
                    return int(val)
        except Exception:
            pass
        return None

    def set(self, target_ms):
        with self.lock:
            if not self.enabled:
                return 0, TVAP_STATUS_UNSUPPORTED
            # Respect user manual change via remote if current setting differs from what we last wrote
            live = self.get()
            if live is not None and self.last_applied_by_us is not None and live != self.last_applied_by_us:
                self.orig_delay_ms = live
                self.current_delay_ms = live
                self.last_applied_by_us = None
                try:
                    with open(self.orig_file, "w") as f:
                        f.write(str(live) + "\n")
                except Exception:
                    pass

            clamped = max(0, min(self.max_ms, int(target_ms)))
            status = TVAP_STATUS_CLAMPED if clamped != int(target_ms) else TVAP_STATUS_OK
            try:
                payload = json.dumps({"category": "sound", "settings": {"avSyncSpeaker": str(clamped)}})
                rc, _ = self.runner([
                    "/usr/bin/luna-send", "-n", "1",
                    "luna://com.webos.settingsservice/setSystemSettings",
                    payload
                ])
                if rc != 0:
                    return self.current_delay_ms, TVAP_STATUS_ERROR
                rb = self.get()
                applied = rb if rb is not None else clamped
                self.current_delay_ms = applied
                self.last_applied_by_us = applied
                return applied, status
            except Exception:
                return self.current_delay_ms, TVAP_STATUS_ERROR

    def restore(self):
        with self.lock:
            if not self.enabled or self.orig_delay_ms is None:
                return
            try:
                payload = json.dumps({"category": "sound", "settings": {"avSyncSpeaker": str(self.orig_delay_ms)}})
                self.runner([
                    "/usr/bin/luna-send", "-n", "1",
                    "luna://com.webos.settingsservice/setSystemSettings",
                    payload
                ])
                self.current_delay_ms = self.orig_delay_ms
                self.last_applied_by_us = None
                if os.path.exists(self.orig_file):
                    os.remove(self.orig_file)
            except Exception:
                pass


class VoxAudioSender:
    def __init__(self, station_ip=DEFAULT_STATION_IP, station_port=DEFAULT_STATION_PORT,
                 backend="alsa", device=DEFAULT_ALSA_DEVICE, sample_rate=48000,
                 silence_rms=DEFAULT_SILENCE_RMS, wake_rms=DEFAULT_WAKE_RMS,
                 silence_timeout=DEFAULT_SILENCE_TIMEOUT, conf_path=DEFAULT_CONF_PATH):
        self.station_ip = station_ip
        self.station_port = station_port
        self.backend = backend.lower()
        self.device = device or DEFAULT_ALSA_DEVICE
        self.sample_rate = sample_rate
        self.silence_rms = silence_rms
        self.wake_rms = wake_rms
        self.silence_timeout = silence_timeout
        self.cfg = load_config(conf_path)

        self.running = False
        self.tv_active = True

        self.tv_volume = 5
        self.station_volume_pct = self.calc_station_volume_pct(5, False)
        self.is_muted = False
        self.vol_lock = threading.Lock()

        # Telemetry from SAK2 / SACK
        self.live_state = 0
        self.live_flags = 0
        self.live_tv_speaker_delay_ms = 0
        self.live_station_delay_ms = 0
        self.live_ltv_ms = 0.0
        self.live_acoustic_err_ms = 0.0
        self.live_confidence = 0.0
        self.last_sack_time = 0.0
        self.last_cmd_seq = -1

        self.tv_actuator = TvDelayActuator(
            enabled=self.cfg.get("tv_delay_enabled", False),
            max_ms=self.cfg.get("tv_delay_max_ms", 50),
        )

        # Stats
        self.packets_sent = 0
        self.bytes_sent = 0
        self.sacks_received = 0
        self.silence_pauses = 0

        self.sock = None
        self.capture_proc = None
        self.luna_vol_proc = None
        self.luna_pwr_proc = None

    def calc_station_volume_pct(self, tv_vol, is_muted):
        return calc_station_volume_pct(
            tv_vol,
            is_muted,
            slope=self.cfg["vol_slope"],
            cap=self.cfg["vol_cap"],
            gamma=self.cfg["vol_gamma"],
        )

    def build_syn3_packet(self, seq, sample_index, capture_ts_ms, pcm_chunk,
                          force_muted=None, session_reset=False, low_latency=False):
        """Pack 20-byte SYN3 header + 960-byte PCM (980 B total, zero IPv4 fragmentation)."""
        with self.vol_lock:
            tv_vol = max(0, min(100, int(self.tv_volume)))
            st_pct = max(0, min(100, int(self.station_volume_pct)))
            muted = self.is_muted if force_muted is None else bool(force_muted)

        if muted or tv_vol <= 0:
            st_pct = 0
            muted = True

        flags = 0
        if muted:
            flags |= FLAG_MUTED
        if session_reset:
            flags |= FLAG_SESSION_RESET
        if low_latency:
            flags |= FLAG_LOW_LATENCY

        frame_ms_x2 = max(1, min(255, int(round((len(pcm_chunk) / (48.0 * 4.0)) * 2.0))))
        header = struct.pack(
            ">IIIIBBBB",
            MAGIC_SYN3,
            seq & 0xFFFFFFFF,
            sample_index & 0xFFFFFFFF,
            capture_ts_ms & 0xFFFFFFFF,
            tv_vol & 0xFF,
            st_pct & 0xFF,
            flags & 0xFF,
            frame_ms_x2 & 0xFF,
        )
        return header + pcm_chunk

    def send_mute_burst(self, seq, sample_index, count=5):
        """Send 5x is_muted=1 control packets over ~25ms before pausing TX for instant Station mute."""
        if not self.sock:
            return seq
        silence = b"\x00" * CHUNK_PCM_LEN
        for _ in range(count):
            ts_ms = int(time.monotonic() * 1000) & 0xFFFFFFFF
            pkt = self.build_syn3_packet(seq, sample_index, ts_ms, silence, force_muted=True)
            try:
                self.sock.sendto(pkt, (self.station_ip, self.station_port))
            except Exception:
                pass
            seq = (seq + 1) & 0xFFFFFFFF
            sample_index = (sample_index + CHUNK_FRAMES_48K) & 0xFFFFFFFF
            self.packets_sent += 1
            self.bytes_sent += len(pkt)
            time.sleep(0.005)
        return seq

    def send_tvap(self, applied_ms, status, cmd_seq):
        if not self.sock:
            return
        pkt = struct.pack(">IHBB", MAGIC_TVAP, max(0, int(applied_ms)) & 0xFFFF, status & 0xFF, cmd_seq & 0xFF)
        try:
            self.sock.sendto(pkt, (self.station_ip, self.station_port))
        except Exception:
            pass

    def parse_telemetry_packet(self, data):
        """Parse either SAK2 (18 B) or legacy SACK (12 B, unsigned confidence!)."""
        if len(data) >= 18:
            magic = struct.unpack(">I", data[:4])[0]
            if magic == MAGIC_SAK2:
                _, state, flags, req_tv, st_delay, ltv_t, err_t, conf_pm, cmd_seq, _ = struct.unpack(
                    ">IBBHHHhHBB", data[:18]
                )
                self.live_state = state
                self.live_flags = flags
                self.live_tv_speaker_delay_ms = req_tv
                self.live_station_delay_ms = st_delay
                self.live_ltv_ms = ltv_t / 10.0
                self.live_acoustic_err_ms = err_t / 10.0
                self.live_confidence = conf_pm / 1000.0
                self.last_sack_time = time.monotonic()
                self.sacks_received += 1

                if cmd_seq != 0 and cmd_seq != self.last_cmd_seq:
                    self.last_cmd_seq = cmd_seq
                    applied, status = self.tv_actuator.set(req_tv)
                    self.send_tvap(applied, status, cmd_seq)
                return True

        if len(data) >= 12:
            magic, tv_delay, st_delay, err_tenths, conf_thou = struct.unpack(">IHHhH", data[:12])
            if magic == MAGIC_SACK:
                self.live_tv_speaker_delay_ms = tv_delay
                self.live_station_delay_ms = st_delay
                self.live_acoustic_err_ms = err_tenths / 10.0
                self.live_confidence = conf_thou / 1000.0
                self.last_sack_time = time.monotonic()
                self.sacks_received += 1
                return True
        return False

    def _start_volume_listener(self):
        def listener():
            cmd = ["/usr/bin/luna-send", "-i", "luna://com.webos.service.audio/master/getVolume", '{"subscribe":true}']
            while self.running:
                try:
                    self.luna_vol_proc = subprocess.Popen(
                        cmd, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, bufsize=1
                    )
                    for line in self.luna_vol_proc.stdout:
                        if not self.running:
                            break
                        line = line.strip()
                        if not line or not line.startswith("{"):
                            continue
                        try:
                            data = json.loads(line)
                            if data.get("returnValue"):
                                vs = data.get("volumeStatus")
                                if isinstance(vs, dict):
                                    vol = vs.get("volume")
                                    mute = vs.get("muteStatus", False)
                                else:
                                    vol = data.get("volume")
                                    mute = data.get("muteStatus", False)
                                if vol is not None:
                                    with self.vol_lock:
                                        self.tv_volume = int(vol)
                                        self.is_muted = bool(mute)
                                        self.station_volume_pct = self.calc_station_volume_pct(
                                            self.tv_volume, self.is_muted
                                        )
                        except Exception:
                            pass
                except Exception:
                    if self.running:
                        time.sleep(2)

        threading.Thread(target=listener, daemon=True).start()

    def _start_power_listener(self):
        def listener():
            cmd = ["/usr/bin/luna-send", "-i", "luna://com.webos.service.tvpower/power/getPowerState", '{"subscribe":true}']
            while self.running:
                try:
                    self.luna_pwr_proc = subprocess.Popen(
                        cmd, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, bufsize=1
                    )
                    for line in self.luna_pwr_proc.stdout:
                        if not self.running:
                            break
                        line = line.strip()
                        if not line or not line.startswith("{"):
                            continue
                        try:
                            data = json.loads(line)
                            if data.get("returnValue"):
                                state = data.get("state", "Active")
                                was_active = self.tv_active
                                self.tv_active = (state == "Active")
                                if was_active and not self.tv_active:
                                    self.tv_actuator.restore()
                        except Exception:
                            pass
                except Exception:
                    if self.running:
                        time.sleep(2)

        threading.Thread(target=listener, daemon=True).start()

    def _start_sack_listener(self):
        def listener():
            while self.running:
                try:
                    r, _, _ = select.select([self.sock], [], [], 0.5)
                    if not r:
                        # Rollback TV delay if SAK2 lost > 5s
                        if self.last_sack_time > 0 and (time.monotonic() - self.last_sack_time) > 5.0:
                            self.tv_actuator.restore()
                        continue
                    data, addr = self.sock.recvfrom(128)
                    if addr and addr[0] != self.station_ip:
                        continue
                    self.parse_telemetry_packet(data)
                except Exception:
                    pass

        threading.Thread(target=listener, daemon=True).start()

    def _start_capture_proc(self):
        if self.backend == "alsa":
            cmd = [
                "/usr/bin/arecord",
                "-D", self.device,
                "-f", "S16_LE",
                "-c", "2",
                "-r", str(self.sample_rate),
                "--period-size=240",
                "--buffer-size=960",
                "-t", "raw",
            ]
        else:
            cmd = [
                "/usr/bin/parec",
                f"--device={self.device}",
                f"--rate={self.sample_rate}",
                "--format=s16le",
                "--channels=2",
                "--latency-msec=5",
                "--process-time-msec=5",
                "--raw",
            ]
        return subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)

    def _read_exact_with_watchdog(self, stream, nbytes, timeout_sec=3.0):
        buf = bytearray()
        deadline = time.monotonic() + timeout_sec
        fd = stream.fileno()
        while len(buf) < nbytes and self.running:
            rem = deadline - time.monotonic()
            if rem <= 0:
                return None  # Watchdog timeout!
            r, _, _ = select.select([fd], [], [], min(0.25, rem))
            if not r:
                continue
            chunk = os.read(fd, nbytes - len(buf))
            if not chunk:
                return b""
            buf.extend(chunk)
        return bytes(buf)

    def run(self, max_duration_sec=None):
        self.running = True
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        try:
            self.sock.setsockopt(socket.IPPROTO_IP, socket.IP_TOS, 0xB8)  # DSCP EF
        except Exception:
            pass
        self.sock.setblocking(False)

        # Initial synchronous volume query
        try:
            init_res = subprocess.run(
                ["/usr/bin/luna-send", "-n", "1", "luna://com.webos.service.audio/master/getVolume", "{}"],
                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, timeout=1.0
            )
            if init_res.stdout:
                init_data = json.loads(init_res.stdout.strip())
                vs = init_data.get("volumeStatus")
                if isinstance(vs, dict):
                    vol = vs.get("volume")
                    mute = vs.get("muteStatus", False)
                    if vol is not None:
                        with self.vol_lock:
                            self.tv_volume = int(vol)
                            self.is_muted = bool(mute)
                            self.station_volume_pct = self.calc_station_volume_pct(self.tv_volume, self.is_muted)
        except Exception:
            pass

        self._start_volume_listener()
        self._start_power_listener()
        self._start_sack_listener()

        seq = 0
        sample_index = 0
        start_time = time.monotonic()
        last_audio_time = start_time
        last_stat_time = start_time
        is_transmitting = False
        need_session_reset = True

        print(f"[*] VoxAudioSender v3.0 (SYN3 5ms/980B) -> {self.station_ip}:{self.station_port} on {self.device}", flush=True)

        # Disable cyclic GC in hot streaming loop (manual gc.collect() during silence pauses)
        gc.disable()

        try:
            while self.running:
                if max_duration_sec and (time.monotonic() - start_time) >= max_duration_sec:
                    break

                try:
                    self.capture_proc = self._start_capture_proc()
                except Exception as e:
                    print(f"[!] Failed to launch capture ({e}), retrying in 2s...", flush=True)
                    time.sleep(2)
                    continue

                try:
                    while self.running:
                        if max_duration_sec and (time.monotonic() - start_time) >= max_duration_sec:
                            break

                        chunk = self._read_exact_with_watchdog(self.capture_proc.stdout, CHUNK_PCM_LEN, timeout_sec=3.0)
                        # Timestamp taken AFTER blocking ALSA read!
                        now = time.monotonic()

                        if chunk is None:
                            print("[!] arecord watchdog timeout (>3s without data). Restarting capture...", flush=True)
                            break
                        if len(chunk) < CHUNK_PCM_LEN:
                            print("[!] Capture EOF/exit. Re-spawning...", flush=True)
                            break

                        rms = calc_rms(chunk)
                        # Working wake_rms / silence_rms hysteresis!
                        if is_transmitting:
                            if rms >= self.silence_rms:
                                last_audio_time = now
                        else:
                            if rms >= self.wake_rms:
                                last_audio_time = now

                        silence_duration = now - last_audio_time
                        with self.vol_lock:
                            muted_or_zero = self.is_muted or (self.tv_volume <= 0)

                        should_stream = self.tv_active and (silence_duration < self.silence_timeout) and not muted_or_zero

                        if should_stream:
                            if not is_transmitting:
                                is_transmitting = True
                                need_session_reset = True
                                print(f"[+] Audio activity resumed (RMS={rms}). SYN3 streaming active.", flush=True)

                            cap_ts_ms = int(now * 1000) & 0xFFFFFFFF
                            pkt = self.build_syn3_packet(
                                seq, sample_index, cap_ts_ms, chunk,
                                session_reset=need_session_reset
                            )
                            need_session_reset = False
                            try:
                                self.sock.sendto(pkt, (self.station_ip, self.station_port))
                            except Exception:
                                pass

                            seq = (seq + 1) & 0xFFFFFFFF
                            sample_index = (sample_index + CHUNK_FRAMES_48K) & 0xFFFFFFFF
                            self.packets_sent += 1
                            self.bytes_sent += len(pkt)
                        else:
                            if is_transmitting:
                                is_transmitting = False
                                self.silence_pauses += 1
                                if muted_or_zero:
                                    seq = self.send_mute_burst(seq, sample_index, count=5)
                                    sample_index = (sample_index + 5 * CHUNK_FRAMES_48K) & 0xFFFFFFFF
                                reason = "Standby" if not self.tv_active else (
                                    "Muted/Vol0" if muted_or_zero else f"Silence ({silence_duration:.1f}s)"
                                )
                                print(f"[-] Pausing SYN3 TX ({reason}) -> Station ALSA released.", flush=True)
                                gc.collect()

                        if now - last_stat_time >= 5.0:
                            last_stat_time = now
                            elapsed = now - start_time
                            pps = self.packets_sent / elapsed if elapsed > 0 else 0
                            st_name = STATE_NAMES.get(self.live_state, str(self.live_state))
                            tx_str = "STREAMING" if is_transmitting else "IDLE_PAUSED"
                            print(
                                f"[{tx_str}] TX={self.packets_sent} ({pps:.1f} pps, RMS={rms}) | "
                                f"TV={self.tv_volume} ST={self.station_volume_pct}% Mute={self.is_muted} | "
                                f"SAK2: {st_name} Dst={self.live_station_delay_ms}ms Ltv={self.live_ltv_ms:.1f}ms "
                                f"Err={self.live_acoustic_err_ms:+.1f}ms Conf={self.live_confidence:.2f} ({self.sacks_received} acks)",
                                flush=True
                            )
                except Exception as e:
                    print(f"[!] Capture loop error: {e}", flush=True)
                finally:
                    if self.capture_proc:
                        try:
                            self.capture_proc.terminate()
                            self.capture_proc.wait(timeout=1)
                        except Exception:
                            try:
                                self.capture_proc.kill()
                            except Exception:
                                pass
                        self.capture_proc = None

                if self.running:
                    time.sleep(0.5)
        finally:
            gc.enable()
            self.stop()

    def stop(self):
        self.running = False
        self.tv_actuator.restore()
        for proc_attr in ("capture_proc", "luna_vol_proc", "luna_pwr_proc"):
            proc = getattr(self, proc_attr, None)
            if proc:
                try:
                    proc.terminate()
                    proc.wait(timeout=1)
                except Exception:
                    try:
                        proc.kill()
                    except Exception:
                        pass
                setattr(self, proc_attr, None)
        if self.sock:
            try:
                self.sock.close()
            except Exception:
                pass
            self.sock = None
        print(f"[*] VoxAudioSender v3.0 stopped cleanly. Total packets: {self.packets_sent}.", flush=True)


def main():
    parser = argparse.ArgumentParser(description="webOS to Yandex Station Audio Sender v3.0 (SYN3)")
    parser.add_argument("--ip", default=DEFAULT_STATION_IP)
    parser.add_argument("--port", type=int, default=DEFAULT_STATION_PORT)
    parser.add_argument("--backend", default="alsa", choices=["alsa", "pulse"])
    parser.add_argument("--device", default=DEFAULT_ALSA_DEVICE)
    parser.add_argument("--duration", type=float, default=None)
    parser.add_argument("--silence-rms", type=int, default=DEFAULT_SILENCE_RMS)
    parser.add_argument("--wake-rms", type=int, default=DEFAULT_WAKE_RMS)
    parser.add_argument("--silence-timeout", type=float, default=DEFAULT_SILENCE_TIMEOUT)
    parser.add_argument("--conf", default=DEFAULT_CONF_PATH)
    parser.add_argument("--pidfile", default=None)
    args = parser.parse_args()

    if args.pidfile:
        try:
            with open(args.pidfile, "w") as pf:
                pf.write(str(os.getpid()) + "\n")
        except Exception as e:
            print(f"[!] Warning: pidfile write failed: {e}", file=sys.stderr)

    sender = VoxAudioSender(
        station_ip=args.ip,
        station_port=args.port,
        backend=args.backend,
        device=args.device,
        silence_rms=args.silence_rms,
        wake_rms=args.wake_rms,
        silence_timeout=args.silence_timeout,
        conf_path=args.conf,
    )

    def handle_signal(sig, frame):
        print(f"\n[*] Signal {sig} caught, stopping...", flush=True)
        sender.stop()
        if args.pidfile and os.path.exists(args.pidfile):
            try:
                os.remove(args.pidfile)
            except Exception:
                pass
        sys.exit(0)

    signal.signal(signal.SIGINT, handle_signal)
    signal.signal(signal.SIGTERM, handle_signal)

    try:
        sender.run(max_duration_sec=args.duration)
    finally:
        if args.pidfile and os.path.exists(args.pidfile):
            try:
                os.remove(args.pidfile)
            except Exception:
                pass


if __name__ == "__main__":
    main()
