/*
 * alsa_raw_tap.c
 *
 * Non-blocking ALSA capture tap for Yandex Station Mini 2 (ARM32 EABI5).
 * Intercepts standard ALSA libasound calls (snd_pcm_open, snd_pcm_hw_params_set_*,
 * snd_pcm_readi, snd_pcm_close) to mirror the 6-channel 48 kHz S16_LE capture stream
 * (raw_mic_0..3 + raw_spk_0..1) into a non-blocking Unix domain datagram socket
 * (/tmp/audiosync_mic.sock) for acoustic latency synchronization.
 *
 * Original ALSA buffers, return values, and errno are preserved untouched.
 */

#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/types.h>
#include <sys/un.h>
#include <time.h>
#include <unistd.h>

typedef struct _snd_pcm snd_pcm_t;
typedef struct _snd_pcm_hw_params snd_pcm_hw_params_t;
typedef long snd_pcm_sframes_t;
typedef unsigned long snd_pcm_uframes_t;

typedef enum _snd_pcm_stream {
    SND_PCM_STREAM_PLAYBACK = 0,
    SND_PCM_STREAM_CAPTURE = 1
} snd_pcm_stream_t;

#define TAP_SOCK_PATH "/tmp/audiosync_mic.sock"
#define TAP_MAGIC 0x4D544150u /* 'MTAP' */
#define DECIM_FACTOR 3        /* 48000 Hz -> 16000 Hz */
#define MAX_DECIM_FRAMES 256  /* Up to 512 input frames -> 171 output frames */

/*
 * Packet sent over /tmp/audiosync_mic.sock (Unix DGRAM, non-blocking):
 * - magic:       0x4D544150 ('MTAP')
 * - seq:         monotonic packet counter
 * - mono_time_ns: CLOCK_MONOTONIC timestamp at snd_pcm_readi completion
 * - sample_rate: 16000
 * - channels:    3 (ch0 = mic_diff_front_back, ch1 = mic_omni_sum, ch2 = spk_loopback)
 * - frame_count: number of 16 kHz frames in this packet (typically 170 or 171 for 512 input frames)
 * - pcm:         interleaved S16_LE [ch0, ch1, ch2] * frame_count
 */
typedef struct __attribute__((packed)) {
    uint32_t magic;
    uint32_t seq;
    uint64_t mono_time_ns;
    uint16_t sample_rate;
    uint16_t channels;
    uint16_t frame_count;
    uint16_t reserved;
    int16_t  pcm[MAX_DECIM_FRAMES * 3];
} tap_packet_t;

static int (*real_snd_pcm_open)(snd_pcm_t **pcm, const char *name, snd_pcm_stream_t stream, int mode) = NULL;
static snd_pcm_sframes_t (*real_snd_pcm_readi)(snd_pcm_t *pcm, void *buffer, snd_pcm_uframes_t size) = NULL;
static int (*real_snd_pcm_close)(snd_pcm_t *pcm) = NULL;

static snd_pcm_t *g_capture_pcm = NULL;
static int g_sock_fd = -1;
static struct sockaddr_un g_dest_addr;
static uint32_t g_seq = 0;
static int g_phase_rem = 0;

static void resolve_symbols(void) {
    if (!real_snd_pcm_open) {
        real_snd_pcm_open = dlsym(RTLD_NEXT, "snd_pcm_open");
    }
    if (!real_snd_pcm_readi) {
        real_snd_pcm_readi = dlsym(RTLD_NEXT, "snd_pcm_readi");
    }
    if (!real_snd_pcm_close) {
        real_snd_pcm_close = dlsym(RTLD_NEXT, "snd_pcm_close");
    }
}

static void ensure_socket(void) {
    if (g_sock_fd >= 0) {
        return;
    }
    g_sock_fd = socket(AF_UNIX, SOCK_DGRAM, 0);
    if (g_sock_fd >= 0) {
        int flags = fcntl(g_sock_fd, F_GETFL, 0);
        if (flags >= 0) {
            fcntl(g_sock_fd, F_SETFL, flags | O_NONBLOCK);
        }
        fcntl(g_sock_fd, F_SETFD, FD_CLOEXEC);
        memset(&g_dest_addr, 0, sizeof(g_dest_addr));
        g_dest_addr.sun_family = AF_UNIX;
        strncpy(g_dest_addr.sun_path, TAP_SOCK_PATH, sizeof(g_dest_addr.sun_path) - 1);
    }
}

static inline int16_t clamp_s16(int32_t v) {
    if (v > 32767) return 32767;
    if (v < -32768) return -32768;
    return (int16_t)v;
}

int snd_pcm_open(snd_pcm_t **pcm, const char *name, snd_pcm_stream_t stream, int mode) {
    resolve_symbols();
    if (!real_snd_pcm_open) {
        errno = ENOSYS;
        return -ENOSYS;
    }
    int ret = real_snd_pcm_open(pcm, name, stream, mode);
    if (ret == 0 && pcm && *pcm && stream == SND_PCM_STREAM_CAPTURE) {
        /* Remember the 6-channel default/multi capture handle opened by audiod */
        if (!name || strcmp(name, "default") == 0 || strstr(name, "multi") != NULL) {
            g_capture_pcm = *pcm;
            g_phase_rem = 0;
            ensure_socket();
        }
    }
    return ret;
}

int snd_pcm_close(snd_pcm_t *pcm) {
    resolve_symbols();
    if (pcm && pcm == g_capture_pcm) {
        g_capture_pcm = NULL;
    }
    if (!real_snd_pcm_close) {
        errno = ENOSYS;
        return -ENOSYS;
    }
    return real_snd_pcm_close(pcm);
}

snd_pcm_sframes_t snd_pcm_readi(snd_pcm_t *pcm, void *buffer, snd_pcm_uframes_t size) {
    resolve_symbols();
    if (!real_snd_pcm_readi) {
        errno = ENOSYS;
        return -ENOSYS;
    }

    snd_pcm_sframes_t frames = real_snd_pcm_readi(pcm, buffer, size);
    int saved_errno = errno;

    if (frames > 0 && buffer != NULL && (pcm == g_capture_pcm || g_capture_pcm == NULL)) {
        if (g_capture_pcm == NULL) {
            g_capture_pcm = pcm;
            ensure_socket();
        }

        if (g_sock_fd >= 0 && frames <= 768) {
            struct timespec ts;
            clock_gettime(CLOCK_MONOTONIC, &ts);
            uint64_t now_ns = (uint64_t)ts.tv_sec * 1000000000ULL + (uint64_t)ts.tv_nsec;

            /*
             * Input buffer layout from /etc/asound.conf (6 channels S16_LE at 48000 Hz):
             * ch 0: raw_mic_0
             * ch 1: raw_mic_1
             * ch 2: raw_mic_2
             * ch 3: raw_mic_3
             * ch 4: raw_spk_0 (hardware TDM loopback L)
             * ch 5: raw_spk_1 (hardware TDM loopback R)
             *
             * We decimate 48 kHz -> 16 kHz (3:1 boxcar anti-aliasing average) into 3 channels:
             * out_ch 0 (diff): (mic_0 - mic_2) — cancels symmetric internal Station speaker!
             * out_ch 1 (omni): (mic_0 + mic_1 + mic_2 + mic_3) / 4 — full room signal
             * out_ch 2 (spk):  (spk_0 + spk_1) / 2 — exact hardware reference of Station speaker
             */
            const int16_t *in = (const int16_t *)buffer;
            tap_packet_t pkt;
            pkt.magic = TAP_MAGIC;
            pkt.seq = ++g_seq;
            pkt.mono_time_ns = now_ns;
            pkt.sample_rate = 16000;
            pkt.channels = 3;
            pkt.reserved = 0;

            uint16_t out_idx = 0;
            snd_pcm_sframes_t i = g_phase_rem;
            for (; i + 2 < frames && out_idx < MAX_DECIM_FRAMES; i += DECIM_FACTOR) {
                int32_t diff_sum = 0;
                int32_t omni_sum = 0;
                int32_t spk_sum = 0;

                for (int k = 0; k < DECIM_FACTOR; k++) {
                    const int16_t *frame_ptr = in + (i + k) * 6;
                    int32_t m0 = frame_ptr[0];
                    int32_t m1 = frame_ptr[1];
                    int32_t m2 = frame_ptr[2];
                    int32_t m3 = frame_ptr[3];
                    int32_t s0 = frame_ptr[4];
                    int32_t s1 = frame_ptr[5];

                    diff_sum += (m0 - m2);
                    omni_sum += (m0 + m1 + m2 + m3) >> 2;
                    spk_sum  += (s0 + s1) >> 1;
                }

                pkt.pcm[out_idx * 3 + 0] = clamp_s16(diff_sum / DECIM_FACTOR);
                pkt.pcm[out_idx * 3 + 1] = clamp_s16(omni_sum / DECIM_FACTOR);
                pkt.pcm[out_idx * 3 + 2] = clamp_s16(spk_sum / DECIM_FACTOR);
                out_idx++;
            }
            g_phase_rem = (int)(i - frames);
            if (g_phase_rem < 0) {
                g_phase_rem = 0;
            }

            pkt.frame_count = out_idx;
            size_t payload_bytes = 24 + (size_t)out_idx * 3 * sizeof(int16_t);
            (void)sendto(g_sock_fd, &pkt, payload_bytes, MSG_DONTWAIT,
                         (const struct sockaddr *)&g_dest_addr, sizeof(g_dest_addr));
        }
    }

    errno = saved_errno;
    return frames;
}
