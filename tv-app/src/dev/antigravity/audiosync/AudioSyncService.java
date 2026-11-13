package dev.antigravity.audiosync;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.app.usage.UsageEvents;
import android.app.usage.UsageStatsManager;
import android.bluetooth.BluetoothA2dp;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.media.AudioAttributes;
import android.media.AudioDeviceCallback;
import android.media.AudioDeviceInfo;
import android.media.AudioFormat;
import android.media.AudioManager;
import android.media.AudioPlaybackCaptureConfiguration;
import android.media.AudioRecord;
import android.media.AudioTrack;
import android.media.projection.MediaProjection;
import android.media.projection.MediaProjectionManager;
import android.os.Build;
import android.os.IBinder;
import android.os.PowerManager;
import android.os.SystemClock;
import android.util.Log;

import java.net.DatagramPacket;
import java.net.DatagramSocket;
import java.net.InetAddress;
import java.nio.ByteBuffer;
import java.nio.ByteOrder;

public class AudioSyncService extends Service {
    private static final String TAG = "TvAudioSyncService";
    private static final String CHANNEL_ID = "tv_audio_sync_channel";
    private static final int NOTIFICATION_ID = 2026;
    private static final int SACK_MAGIC = 0x5341434B; // 'SACK' from Station Acoustic Controller

    public static final String ACTION_START = "ACTION_START";
    public static final String ACTION_STOP = "ACTION_STOP";
    public static final String EXTRA_RESULT_CODE = "EXTRA_RESULT_CODE";
    public static final String EXTRA_RESULT_DATA = "EXTRA_RESULT_DATA";
    public static final String EXTRA_IP = "EXTRA_IP";

    public static volatile boolean isRunning = false;
    public static volatile String currentIp = "";
    public static volatile boolean isHeadphonesConnected = false;
    public static volatile int globalDelayOffsetMs = 0;

    // Live telemetry reported back by Station Acoustic Controller ('SACK' packets)
    public static volatile int liveStationDelayMs = 150;
    public static volatile int liveTvSpeakerDelayMs = 0;
    public static volatile float liveAcousticErrMs = 0.0f;
    public static volatile float liveAcousticConf = 0.0f;
    private volatile long lastSackTimeMs = 0;

    private volatile int currentTvVolume = 8;
    private volatile int currentStationVolume = 4;
    private volatile boolean currentIsMuted = false;

    private MediaProjection mediaProjection;
    private AudioRecord audioRecord;
    private Thread workerThread;
    private Thread ackListenerThread;
    private Thread appDetectorThread;
    private volatile boolean isCapturing = false;
    private volatile boolean isScreenOff = false;

    // Keepalive track to prevent MediaTek Audio HAL & PatchTrack from entering standby or re-buffering
    private AudioTrack keepAliveTrack;
    private Thread keepAliveThread;
    private volatile boolean isKeepAliveRunning = false;

    private AudioManager audioManager;
    private NotificationManager notificationManager;
    private UsageStatsManager usageStatsManager;
    private boolean isReceiverRegistered = false;

    private String currentForegroundPkg = "";
    private long lastEventsQueryTime = 0;
    private volatile byte currentAppHint = 1; // 1=YouTube/Video, 2=Spotify, 3=Twitch, 4=Kinopub

    private byte detectActiveApp() {
        byte hint = 1; // Default video mode
        if (usageStatsManager != null) {
            try {
                long now = System.currentTimeMillis();
                if (lastEventsQueryTime == 0) {
                    lastEventsQueryTime = now - 1000L * 3600 * 4;
                }
                UsageEvents events = usageStatsManager.queryEvents(lastEventsQueryTime, now);
                if (events != null) {
                    UsageEvents.Event event = new UsageEvents.Event();
                    while (events.hasNextEvent()) {
                        events.getNextEvent(event);
                        if (event.getEventType() == UsageEvents.Event.ACTIVITY_RESUMED) {
                            String pkg = event.getPackageName();
                            if (pkg != null && !pkg.contains("systemui") && !pkg.contains("permissioncontroller") && !pkg.contains("audiosync")) {
                                currentForegroundPkg = pkg;
                            }
                        }
                    }
                }
                lastEventsQueryTime = now - 5000L;

                if (currentForegroundPkg.contains("spotify")) {
                    hint = 2; // Spotify pure audio
                } else if (currentForegroundPkg.contains("twitch")) {
                    hint = 3; // Twitch live
                } else if (currentForegroundPkg.contains("watch.kino") || currentForegroundPkg.contains("kinopub")) {
                    hint = 4; // Kinopoisk / Kinopub
                } else {
                    hint = 1; // YouTube / generic video
                }
            } catch (Exception e) {
                Log.w(TAG, "detectActiveApp error: " + e.getMessage());
            }
        }
        return hint;
    }

    private AudioDeviceCallback audioDeviceCallback = new AudioDeviceCallback() {
        @Override
        public void onAudioDevicesAdded(AudioDeviceInfo[] addedDevices) {
            updateHeadphoneState();
        }

        @Override
        public void onAudioDevicesRemoved(AudioDeviceInfo[] removedDevices) {
            updateHeadphoneState();
        }
    };

    private BroadcastReceiver eventReceiver = new BroadcastReceiver() {
        @Override
        public void onReceive(Context context, Intent intent) {
            if (intent == null || intent.getAction() == null) return;
            String action = intent.getAction();
            if (Intent.ACTION_SCREEN_OFF.equals(action)) {
                Log.i(TAG, "Screen OFF detected: pausing audio stream to Station");
                isScreenOff = true;
            } else if (Intent.ACTION_SCREEN_ON.equals(action)) {
                Log.i(TAG, "Screen ON detected: resuming audio stream to Station");
                isScreenOff = false;
                updateHeadphoneState();
                updateTvVolume();
            } else if ("android.media.VOLUME_CHANGED_ACTION".equals(action)) {
                updateTvVolume();
                Log.i(TAG, "Volume change detected: TV=" + currentTvVolume + " -> Station=" + currentStationVolume + " (muted=" + currentIsMuted + ")");
            } else if (BluetoothA2dp.ACTION_CONNECTION_STATE_CHANGED.equals(action) ||
                       "android.bluetooth.adapter.action.CONNECTION_STATE_CHANGED".equals(action) ||
                       AudioManager.ACTION_AUDIO_BECOMING_NOISY.equals(action)) {
                updateHeadphoneState();
            }
        }
    };

    public static int calculateStationVolume(int tvVol, boolean isMuted) {
        if (isMuted || tvVol <= 0) return 0;
        int stationVol = (tvVol + 1) / 2;
        if (stationVol > 10) stationVol = 10;
        return stationVol;
    }

    private void updateTvVolume() {
        if (audioManager != null) {
            try {
                currentTvVolume = audioManager.getStreamVolume(AudioManager.STREAM_MUSIC);
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
                    currentIsMuted = audioManager.isStreamMute(AudioManager.STREAM_MUSIC);
                } else {
                    currentIsMuted = currentTvVolume == 0;
                }
                currentStationVolume = calculateStationVolume(currentTvVolume, currentIsMuted);
            } catch (Exception e) {
                Log.w(TAG, "updateTvVolume error: " + e.getMessage());
            }
        }
    }

    @Override
    public void onCreate() {
        super.onCreate();
        enforceDefaultDspProfile();
        android.content.SharedPreferences prefs = getSharedPreferences("audiosync", Context.MODE_PRIVATE);
        globalDelayOffsetMs = prefs.getInt("delay_offset", 0);
        audioManager = (AudioManager) getSystemService(Context.AUDIO_SERVICE);
        notificationManager = (NotificationManager) getSystemService(Context.NOTIFICATION_SERVICE);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.LOLLIPOP_MR1) {
            usageStatsManager = (UsageStatsManager) getSystemService(Context.USAGE_STATS_SERVICE);
        }
        createNotificationChannel();

        updateHeadphoneState();
        updateTvVolume();
        applySpeakerDelay(0);

        if (audioManager != null) {
            audioManager.registerAudioDeviceCallback(audioDeviceCallback, null);
        }

        try {
            IntentFilter filter = new IntentFilter();
            filter.addAction(Intent.ACTION_SCREEN_OFF);
            filter.addAction(Intent.ACTION_SCREEN_ON);
            filter.addAction("android.media.VOLUME_CHANGED_ACTION");
            filter.addAction(BluetoothA2dp.ACTION_CONNECTION_STATE_CHANGED);
            filter.addAction("android.bluetooth.adapter.action.CONNECTION_STATE_CHANGED");
            filter.addAction(AudioManager.ACTION_AUDIO_BECOMING_NOISY);
            registerReceiver(eventReceiver, filter);
            isReceiverRegistered = true;
        } catch (Exception e) {
            Log.e(TAG, "Failed to register eventReceiver: " + e.getMessage());
        }
    }

    private boolean checkHeadphonesConnected() {
        if (audioManager == null) return false;
        AudioDeviceInfo[] devices = audioManager.getDevices(AudioManager.GET_DEVICES_OUTPUTS);
        if (devices != null) {
            for (AudioDeviceInfo dev : devices) {
                int type = dev.getType();
                if (type == AudioDeviceInfo.TYPE_BLUETOOTH_A2DP ||
                    type == AudioDeviceInfo.TYPE_BLUETOOTH_SCO ||
                    type == AudioDeviceInfo.TYPE_HEARING_AID ||
                    type == AudioDeviceInfo.TYPE_WIRED_HEADSET ||
                    type == AudioDeviceInfo.TYPE_WIRED_HEADPHONES ||
                    type == AudioDeviceInfo.TYPE_USB_HEADSET) {
                    return true;
                }
            }
        }
        return false;
    }

    private void updateHeadphoneState() {
        boolean connected = checkHeadphonesConnected();
        if (connected != isHeadphonesConnected) {
            isHeadphonesConnected = connected;
            Log.i(TAG, "Headphones state changed: isConnected=" + isHeadphonesConnected);
            if (isRunning && notificationManager != null) {
                notificationManager.notify(NOTIFICATION_ID, buildNotification());
            }
        }
    }

    private void createNotificationChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            NotificationChannel channel = new NotificationChannel(
                    CHANNEL_ID,
                    "TV Audio Sync Streamer",
                    NotificationManager.IMPORTANCE_LOW
            );
            if (notificationManager != null) {
                notificationManager.createNotificationChannel(channel);
            }
        }
    }

    private Notification buildNotification() {
        Intent stopIntent = new Intent(this, AudioSyncService.class);
        stopIntent.setAction(ACTION_STOP);
        PendingIntent pendingStop = PendingIntent.getService(
                this, 0, stopIntent,
                Build.VERSION.SDK_INT >= Build.VERSION_CODES.M ? PendingIntent.FLAG_IMMUTABLE : 0
        );

        Intent openIntent = new Intent(this, MainActivity.class);
        PendingIntent pendingOpen = PendingIntent.getActivity(
                this, 0, openIntent,
                Build.VERSION.SDK_INT >= Build.VERSION_CODES.M ? PendingIntent.FLAG_IMMUTABLE : 0
        );

        String contentText = isHeadphonesConnected
                ? "Наушники подключены (колонка отключена)"
                : "Универсальная акустическая синхронизация активна";

        Notification.Builder builder;
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            builder = new Notification.Builder(this, CHANNEL_ID);
        } else {
            builder = new Notification.Builder(this);
        }
        return builder.setContentTitle("TV Audio Sync")
                .setContentText(contentText)
                .setSmallIcon(android.R.drawable.ic_media_play)
                .setContentIntent(pendingOpen)
                .addAction(android.R.drawable.ic_menu_close_clear_cancel, "Остановить", pendingStop)
                .setOngoing(true)
                .build();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        if (intent == null) return START_NOT_STICKY;

        String action = intent.getAction();
        if (ACTION_START.equals(action)) {
            int resultCode = intent.getIntExtra(EXTRA_RESULT_CODE, 0);
            Intent resultData = intent.getParcelableExtra(EXTRA_RESULT_DATA);
            String ip = intent.getStringExtra(EXTRA_IP);
            if (ip == null || ip.isEmpty()) {
                android.content.SharedPreferences prefs = getSharedPreferences("audiosync", Context.MODE_PRIVATE);
                ip = prefs.getString("station_ip", "192.168.1.132");
            }

            startStreaming(resultCode, resultData, ip);
        } else if (ACTION_STOP.equals(action)) {
            stopStreaming();
            stopSelf();
        }

        return START_NOT_STICKY;
    }

    private void startKeepAliveTrack() {
        if (isKeepAliveRunning) return;
        isKeepAliveRunning = true;
        keepAliveThread = new Thread(new Runnable() {
            @Override
            public void run() {
                try {
                    AudioAttributes attributes = new AudioAttributes.Builder()
                            .setUsage(AudioAttributes.USAGE_MEDIA)
                            .setContentType(AudioAttributes.CONTENT_TYPE_MUSIC)
                            .build();

                    AudioFormat format = new AudioFormat.Builder()
                            .setEncoding(AudioFormat.ENCODING_PCM_16BIT)
                            .setSampleRate(48000)
                            .setChannelMask(AudioFormat.CHANNEL_OUT_STEREO)
                            .build();

                    int bufSize = AudioTrack.getMinBufferSize(48000, AudioFormat.CHANNEL_OUT_STEREO, AudioFormat.ENCODING_PCM_16BIT);
                    if (bufSize < 4096) bufSize = 4096;

                    keepAliveTrack = new AudioTrack.Builder()
                            .setAudioAttributes(attributes)
                            .setAudioFormat(format)
                            .setBufferSizeInBytes(bufSize)
                            .setTransferMode(AudioTrack.MODE_STREAM)
                            .setPerformanceMode(AudioTrack.PERFORMANCE_MODE_NONE)
                            .build();

                    keepAliveTrack.setVolume(0.0f);
                    keepAliveTrack.play();
                    Log.i(TAG, "Keepalive AudioTrack started (MediaTek Audio HAL pinned active)");

                    byte[] zeros = new byte[1920];
                    while (isKeepAliveRunning) {
                        if (isScreenOff) {
                            SystemClock.sleep(100);
                            continue;
                        }
                        int written = keepAliveTrack.write(zeros, 0, zeros.length);
                        if (written < 0) {
                            SystemClock.sleep(100);
                        }
                    }
                } catch (Exception e) {
                    Log.w(TAG, "keepAliveTrack exception: " + e.getMessage());
                } finally {
                    stopKeepAliveTrackInternal();
                }
            }
        }, "AudioKeepAliveThread");
        keepAliveThread.start();
    }

    private synchronized void stopKeepAliveTrackInternal() {
        if (keepAliveTrack != null) {
            try {
                keepAliveTrack.stop();
                keepAliveTrack.release();
            } catch (Exception ignored) {}
            keepAliveTrack = null;
        }
    }

    private synchronized void stopKeepAliveTrack() {
        isKeepAliveRunning = false;
        if (keepAliveThread != null) {
            keepAliveThread.interrupt();
            keepAliveThread = null;
        }
        stopKeepAliveTrackInternal();
    }

    private void startAppDetectorThread() {
        if (appDetectorThread != null) return;
        appDetectorThread = new Thread(new Runnable() {
            @Override
            public void run() {
                while (isCapturing) {
                    try {
                        byte newHint = detectActiveApp();
                        if (newHint != currentAppHint) {
                            Log.i(TAG, "App profile switch: hint " + currentAppHint + " -> " + newHint + " (" + currentForegroundPkg + ")");
                            currentAppHint = newHint;
                            applySpeakerDelay(0);
                        }
                    } catch (Exception e) {
                        Log.w(TAG, "appDetectorThread error: " + e.getMessage());
                    }
                    SystemClock.sleep(2500);
                }
            }
        }, "AppDetectorThread");
        appDetectorThread.start();
    }

    private void startAckListener(final DatagramSocket socket) {
        ackListenerThread = new Thread(new Runnable() {
            @Override
            public void run() {
                byte[] rxBuf = new byte[64];
                ByteBuffer bb = ByteBuffer.wrap(rxBuf);
                bb.order(ByteOrder.BIG_ENDIAN);

                while (isCapturing && socket != null && !socket.isClosed()) {
                    try {
                        DatagramPacket pkt = new DatagramPacket(rxBuf, rxBuf.length);
                        socket.receive(pkt);
                        if (pkt.getLength() >= 12) {
                            bb.clear();
                            int magic = bb.getInt(0);
                            if (magic == SACK_MAGIC) {
                                int reqTvSpkDelay = bb.getShort(4) & 0xFFFF;
                                int stDelay = bb.getShort(6) & 0xFFFF;
                                short errTenths = bb.getShort(8);
                                int confPermille = bb.getShort(10) & 0xFFFF;

                                int effectiveTvSpk = reqTvSpkDelay + globalDelayOffsetMs;
                                if (effectiveTvSpk < 0) effectiveTvSpk = 0;
                                if (effectiveTvSpk > 160) effectiveTvSpk = 160;

                                liveStationDelayMs = stDelay;
                                liveTvSpeakerDelayMs = effectiveTvSpk;
                                liveAcousticErrMs = errTenths / 10.0f;
                                liveAcousticConf = confPermille / 1000.0f;
                                lastSackTimeMs = SystemClock.elapsedRealtime();

                                applySpeakerDelay(effectiveTvSpk);
                            }
                        }
                    } catch (Exception ignored) {
                        if (!isCapturing) break;
                    }
                }
            }
        }, "StationAckListenerThread");
        ackListenerThread.start();
    }

    private synchronized void startStreaming(int resultCode, Intent resultData, final String targetIp) {
        if (isCapturing) return;

        updateHeadphoneState();
        startForeground(NOTIFICATION_ID, buildNotification());

        currentIp = targetIp;
        isRunning = true;
        isScreenOff = false;

        MediaProjectionManager manager = (MediaProjectionManager) getSystemService(Context.MEDIA_PROJECTION_SERVICE);
        if (manager == null || resultData == null) {
            Log.e(TAG, "MediaProjectionManager is null or resultData is null");
            stopSelf();
            return;
        }

        mediaProjection = manager.getMediaProjection(resultCode, resultData);
        if (mediaProjection == null) {
            Log.e(TAG, "Failed to get MediaProjection");
            stopSelf();
            return;
        }

        final AudioPlaybackCaptureConfiguration config = new AudioPlaybackCaptureConfiguration.Builder(mediaProjection)
                .addMatchingUsage(AudioAttributes.USAGE_MEDIA)
                .addMatchingUsage(AudioAttributes.USAGE_GAME)
                .addMatchingUsage(AudioAttributes.USAGE_UNKNOWN)
                .build();

        final AudioFormat audioFormat = new AudioFormat.Builder()
                .setEncoding(AudioFormat.ENCODING_PCM_16BIT)
                .setSampleRate(48000)
                .setChannelMask(AudioFormat.CHANNEL_IN_STEREO)
                .build();

        int minBuf = AudioRecord.getMinBufferSize(48000, AudioFormat.CHANNEL_IN_STEREO, AudioFormat.ENCODING_PCM_16BIT);
        final int bufferSize = minBuf < 4096 ? 4096 : minBuf;

        audioRecord = new AudioRecord.Builder()
                .setAudioPlaybackCaptureConfig(config)
                .setAudioFormat(audioFormat)
                .setBufferSizeInBytes(bufferSize)
                .build();

        if (audioRecord.getState() != AudioRecord.STATE_INITIALIZED) {
            Log.e(TAG, "AudioRecord failed to initialize");
            stopStreaming();
            stopSelf();
            return;
        }

        isCapturing = true;
        audioRecord.startRecording();
        startKeepAliveTrack();
        Log.i(TAG, "Universal AudioPlaybackCapture started (Zero App Rules). Target UDP: " + targetIp + ":12345");

        workerThread = new Thread(new Runnable() {
            @Override
            public void run() {
                DatagramSocket socket = null;
                try {
                    socket = new DatagramSocket();
                    startAckListener(socket);
                    startAppDetectorThread();
                    InetAddress dest = InetAddress.getByName(targetIp);
                    int port = 12345;

                    byte[] pcmBuffer = new byte[1920];
                    byte[] sendBuffer = new byte[16 + 1920];
                    ByteBuffer bb = ByteBuffer.wrap(sendBuffer);
                    bb.order(ByteOrder.BIG_ENDIAN);

                    if (audioRecord != null && Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
                        byte[] drainBuf = new byte[4096];
                        while (audioRecord.read(drainBuf, 0, drainBuf.length, AudioRecord.READ_NON_BLOCKING) > 0) {
                            // Discard pre-existing backlog
                        }
                    }

                    long packetCount = 0;
                    long lastLogTime = System.currentTimeMillis();
                    updateTvVolume();
                    currentAppHint = detectActiveApp();
                    applySpeakerDelay(0);

                    while (isCapturing) {
                        if (audioRecord == null) {
                            SystemClock.sleep(20);
                            continue;
                        }

                        int read = audioRecord.read(pcmBuffer, 0, pcmBuffer.length);
                        if (read > 0) {
                            if (isScreenOff || isHeadphonesConnected) {
                                continue;
                            }

                            long nowElapsed = SystemClock.elapsedRealtime();

                            // 16-byte SYNC header + PCM data:
                            // Byte 15 sends App Profile Hint (1: YouTube, 2: Spotify, 3: Twitch, 4: Kinopub)
                            bb.clear();
                            bb.putInt(0x53594E43); // 'SYNC' magic
                            bb.putInt((int) packetCount);
                            bb.putInt((int) (SystemClock.elapsedRealtime() & 0xFFFFFFFFL));
                            bb.put((byte) ((currentTvVolume & 0x7F) | 0x80));
                            bb.put((byte) currentStationVolume);
                            bb.put((byte) (currentIsMuted ? 1 : 0));
                            bb.put(currentAppHint); // Byte 15: App Profile Hint (updated smoothly in background)
                            bb.put(pcmBuffer, 0, read);

                            DatagramPacket packet = new DatagramPacket(sendBuffer, 16 + read, dest, port);
                            socket.send(packet);
                            packetCount++;

                            if (lastSackTimeMs > 0 && (nowElapsed - lastSackTimeMs) > 5000) {
                                int fallbackDelay = 0;
                                if (liveTvSpeakerDelayMs != fallbackDelay) {
                                    Log.w(TAG, "No SACK from Station for >5s: resetting TV speaker delay to 0 ms");
                                    liveTvSpeakerDelayMs = fallbackDelay;
                                    applySpeakerDelay(fallbackDelay);
                                }
                            }

                            long now = System.currentTimeMillis();
                            if (now - lastLogTime >= 5000) {
                                Log.i(TAG, String.format("Stream stats: %d pkts (appHint=%d) | StationDelay=%dms, TvSpkDelay=%dms, Err=%.1fms (conf=%.2f)",
                                        packetCount, currentAppHint, liveStationDelayMs, liveTvSpeakerDelayMs, liveAcousticErrMs, liveAcousticConf));
                                lastLogTime = now;
                            }
                        } else if (read < 0) {
                            Log.w(TAG, "AudioRecord read error: " + read + ", attempting auto-recovery...");
                            try {
                                if (audioRecord != null) {
                                    audioRecord.stop();
                                    audioRecord.release();
                                }
                            } catch (Exception ignored) {}
                            audioRecord = null;

                            SystemClock.sleep(500);
                            if (!isCapturing) break;

                            try {
                                audioRecord = new AudioRecord.Builder()
                                        .setAudioPlaybackCaptureConfig(config)
                                        .setAudioFormat(audioFormat)
                                        .setBufferSizeInBytes(bufferSize)
                                        .build();
                                if (audioRecord.getState() == AudioRecord.STATE_INITIALIZED) {
                                    audioRecord.startRecording();
                                    Log.i(TAG, "AudioRecord auto-recovered successfully!");
                                } else {
                                    audioRecord = null;
                                    SystemClock.sleep(1000);
                                }
                            } catch (Exception recEx) {
                                audioRecord = null;
                                SystemClock.sleep(1000);
                            }
                        }
                    }
                } catch (Exception e) {
                    Log.e(TAG, "Streaming loop exception: " + e.getMessage(), e);
                } finally {
                    if (socket != null && !socket.isClosed()) {
                        socket.close();
                    }
                }
            }
        }, "AudioSyncUdpThread");
        workerThread.start();
    }

    private synchronized void stopStreaming() {
        applySpeakerDelay(0);
        isCapturing = false;
        isRunning = false;
        currentIp = "";

        stopKeepAliveTrack();

        if (workerThread != null) {
            workerThread.interrupt();
            workerThread = null;
        }
        if (ackListenerThread != null) {
            ackListenerThread.interrupt();
            ackListenerThread = null;
        }
        if (appDetectorThread != null) {
            appDetectorThread.interrupt();
            appDetectorThread = null;
        }

        if (audioRecord != null) {
            try {
                audioRecord.stop();
                audioRecord.release();
            } catch (Exception ignored) {}
            audioRecord = null;
        }

        if (mediaProjection != null) {
            try {
                mediaProjection.stop();
            } catch (Exception ignored) {}
            mediaProjection = null;
        }

        stopForeground(true);
        Log.i(TAG, "AudioPlaybackCapture stopped cleanly.");
    }

    @Override
    public void onDestroy() {
        stopStreaming();
        if (audioManager != null) {
            try {
                audioManager.unregisterAudioDeviceCallback(audioDeviceCallback);
            } catch (Exception ignored) {}
        }
        if (isReceiverRegistered) {
            try {
                unregisterReceiver(eventReceiver);
                isReceiverRegistered = false;
            } catch (Exception ignored) {}
        }
        super.onDestroy();
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }

    private void enforceDefaultDspProfile() {
        try {
            android.content.ContentResolver cr = getContentResolver();
            android.provider.Settings.Global.putInt(cr, "sound_advanced_surround_virtualizer", 1);
            android.provider.Settings.Global.putInt(cr, "sound_advanced_dolby_atmos", 1);
            android.provider.Settings.Global.putInt(cr, "sound_bass_enhancement", 1);
            android.provider.Settings.Global.putInt(cr, "sound_dts_virtual_x", 0);
            android.provider.Settings.Global.putInt(cr, "sound_dts_studio_surround", 0);
            android.provider.Settings.Global.putInt(cr, "sound_dbx_total_surround", 0);
            android.provider.Settings.Global.putInt(cr, "sound_sonic_emotion", 0);
            android.provider.Settings.Global.putInt(cr, "sound_auto_volume_leveling", 0);
            android.provider.Settings.Global.putInt(cr, "sound_dts_limiter", 0);
        } catch (Throwable ignored) {}
    }

    private int currentSpeakerDelay = -1;

    private synchronized void applySpeakerDelay(int delayMs) {
        if (currentSpeakerDelay >= 0 && delayMs != 0 && Math.abs(currentSpeakerDelay - delayMs) < 5) {
            return;
        }
        if (currentSpeakerDelay != delayMs) {
            currentSpeakerDelay = delayMs;
            try {
                if (audioManager != null) {
                    audioManager.setParameters("speaker_audio_delay=" + delayMs);
                }
                Log.i(TAG, "Applied speaker_audio_delay=" + delayMs + " ms (controlled by Station Acoustic Sync)");
            } catch (Throwable t) {
                Log.w(TAG, "Failed to apply speaker_audio_delay: " + t.getMessage());
            }
        }
    }
}
