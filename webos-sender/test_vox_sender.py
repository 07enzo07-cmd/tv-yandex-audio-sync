#!/usr/bin/env python3
import json
import os
import socket
import struct
import tempfile
import unittest

from vox_sender import (
    CHUNK_PCM_LEN,
    FLAG_LOW_LATENCY,
    FLAG_MUTED,
    FLAG_SESSION_RESET,
    MAGIC_SACK,
    MAGIC_SAK2,
    MAGIC_SYN3,
    MAGIC_TVAP,
    PACKET_TOTAL_LEN,
    TVAP_STATUS_CLAMPED,
    TVAP_STATUS_OK,
    TVAP_STATUS_UNSUPPORTED,
    TvDelayActuator,
    VoxAudioSender,
    calc_rms,
    calc_station_volume_pct,
)


class TestVoxAudioSenderV3(unittest.TestCase):
    def setUp(self):
        self.sender = VoxAudioSender(conf_path="/nonexistent")

    def test_volume_mapping(self):
        # Phase 4 spec: TV=12 -> 80% (level 8), TV=15+ -> 100%, no cap at 6!
        expected = {
            0: 0,
            1: 7,
            4: 27,
            6: 40,
            8: 53,
            9: 60,
            10: 67,
            11: 73,
            12: 80,
            13: 87,
            15: 100,
            20: 100,
        }
        for tv, pct in expected.items():
            self.assertEqual(calc_station_volume_pct(tv, False), pct, f"TV={tv}")
        # Mute override
        self.assertEqual(calc_station_volume_pct(12, True), 0)
        self.assertEqual(calc_station_volume_pct(6, True), 0)

    def test_syn3_packet_structure_no_ipv4_frag(self):
        self.assertLess(PACKET_TOTAL_LEN + 28, 1500)  # IPv4+UDP < MTU 1500
        self.assertEqual(PACKET_TOTAL_LEN, 980)
        dummy_pcm = b"\x01\x02" * (CHUNK_PCM_LEN // 2)
        self.sender.tv_volume = 12
        self.sender.station_volume_pct = 80
        self.sender.is_muted = False

        pkt = self.sender.build_syn3_packet(77, 77 * 240, 123456, dummy_pcm, session_reset=True)
        self.assertEqual(len(pkt), 980)
        magic, seq, sidx, ts, tv_v, st_pct, flags, fms2 = struct.unpack(">IIIIBBBB", pkt[:20])
        self.assertEqual(magic, MAGIC_SYN3)
        self.assertEqual(seq, 77)
        self.assertEqual(sidx, 77 * 240)
        self.assertEqual(ts, 123456)
        self.assertEqual(tv_v, 12)
        self.assertEqual(st_pct, 80)
        self.assertEqual(flags, FLAG_SESSION_RESET)
        self.assertEqual(fms2, 10)  # 5.0 ms * 2
        self.assertEqual(pkt[20:], dummy_pcm)

    def test_sak2_and_legacy_sack_unsigned_conf(self):
        # Legacy SACK with unsigned confidence > 32767 would have been negative with old 'h'!
        sack_bytes = struct.pack(">IHHhH", MAGIC_SACK, 0, 65, -25, 950)
        self.assertTrue(self.sender.parse_telemetry_packet(sack_bytes))
        self.assertEqual(self.sender.live_station_delay_ms, 65)
        self.assertAlmostEqual(self.sender.live_acoustic_err_ms, -2.5)
        self.assertAlmostEqual(self.sender.live_confidence, 0.95)

        # SAK2 18 bytes
        sak2_bytes = struct.pack(">IBBHHHhHBB", MAGIC_SAK2, 3, 3, 15, 55, 125, -15, 980, 0, 0)
        self.assertTrue(self.sender.parse_telemetry_packet(sak2_bytes))
        self.assertEqual(self.sender.live_state, 3)
        self.assertEqual(self.sender.live_station_delay_ms, 55)
        self.assertAlmostEqual(self.sender.live_ltv_ms, 12.5)
        self.assertAlmostEqual(self.sender.live_acoustic_err_ms, -1.5)
        self.assertAlmostEqual(self.sender.live_confidence, 0.98)

    def test_mute_burst_sends_5_packets(self):
        sent = []

        class DummySock:
            def sendto(self, data, addr):
                sent.append((data, addr))

        sender = VoxAudioSender(station_ip="127.0.0.1", station_port=12345, conf_path="/nonexistent")
        sender.sock = DummySock()
        next_seq = sender.send_mute_burst(10, 2400, count=5)
        self.assertEqual(next_seq, 15)
        self.assertEqual(len(sent), 5)

        received = []
        for data, addr in sent:
            self.assertEqual(addr, ("127.0.0.1", 12345))
            magic, seq, _, _, _, st_pct, flags, _ = struct.unpack(">IIIIBBBB", data[:20])
            self.assertEqual(magic, MAGIC_SYN3)
            self.assertEqual(st_pct, 0)
            self.assertTrue(flags & FLAG_MUTED)
            received.append(seq)
        self.assertEqual(received, [10, 11, 12, 13, 14])

    def test_tv_delay_actuator_mocks_and_restore(self):
        state = {"avSyncSpeaker": "0"}

        def mock_runner(args):
            if "getSystemSettings" in args[3]:
                return 0, json.dumps({"returnValue": True, "settings": {"avSyncSpeaker": state["avSyncSpeaker"]}})
            if "setSystemSettings" in args[3]:
                payload = json.loads(args[4])
                state["avSyncSpeaker"] = payload["settings"]["avSyncSpeaker"]
                return 0, json.dumps({"returnValue": True})
            return 1, ""

        with tempfile.TemporaryDirectory() as td:
            orig_f = os.path.join(td, "orig")
            act = TvDelayActuator(enabled=True, max_ms=40, orig_file=orig_f, runner=mock_runner)
            self.assertEqual(act.orig_delay_ms, 0)

            applied, status = act.set(25)
            self.assertEqual((applied, status), (25, TVAP_STATUS_OK))
            self.assertEqual(state["avSyncSpeaker"], "25")

            applied, status = act.set(60)
            self.assertEqual((applied, status), (40, TVAP_STATUS_CLAMPED))

            act.restore()
            self.assertEqual(state["avSyncSpeaker"], "0")
            self.assertFalse(os.path.exists(orig_f))

            disabled = TvDelayActuator(enabled=False, runner=mock_runner)
            self.assertEqual(disabled.set(20), (0, TVAP_STATUS_UNSUPPORTED))

    def test_calc_rms(self):
        self.assertEqual(calc_rms(b"\x00" * CHUNK_PCM_LEN), 0)
        tone = struct.pack("<h", 1200) * (CHUNK_PCM_LEN // 2)
        self.assertAlmostEqual(calc_rms(tone), 1200, delta=20)

    def test_foreground_app_low_latency_and_session_reset(self):
        dummy_pcm = b"\x01\x02" * (CHUNK_PCM_LEN // 2)
        # Initial app: YouTube -> no session_reset yet, low_latency=False
        self.assertTrue(self.sender.handle_foreground_app_event("youtube.leanback.v4"))
        self.assertFalse(self.sender.low_latency_mode)
        self.assertFalse(self.sender.pending_session_reset)
        pkt_yt = self.sender.build_syn3_packet(1, 240, 1000, dummy_pcm)
        self.assertEqual(pkt_yt[18] & FLAG_LOW_LATENCY, 0)

        # Switch to HDMI2 (PS5) -> low_latency=True AND pending_session_reset=True
        self.assertTrue(self.sender.handle_foreground_app_event("com.webos.app.hdmi2"))
        self.assertTrue(self.sender.low_latency_mode)
        self.assertTrue(self.sender.pending_session_reset)
        pkt_hdmi = self.sender.build_syn3_packet(2, 480, 1005, dummy_pcm, session_reset=True)
        self.assertEqual(pkt_hdmi[18] & FLAG_LOW_LATENCY, FLAG_LOW_LATENCY)
        self.assertEqual(pkt_hdmi[18] & FLAG_SESSION_RESET, FLAG_SESSION_RESET)

        # Duplicate event for same app does not re-trigger reset
        self.sender.pending_session_reset = False
        self.assertFalse(self.sender.handle_foreground_app_event("com.webos.app.hdmi2"))
        self.assertFalse(self.sender.pending_session_reset)

        # Switch to Spotify -> low_latency=False AND pending_session_reset=True
        self.assertTrue(self.sender.handle_foreground_app_event("spotify-beehive"))
        self.assertFalse(self.sender.low_latency_mode)
        self.assertTrue(self.sender.pending_session_reset)
        pkt_sp = self.sender.build_syn3_packet(3, 720, 1010, dummy_pcm)
        self.assertEqual(pkt_sp[18] & FLAG_LOW_LATENCY, 0)


if __name__ == "__main__":
    unittest.main()
