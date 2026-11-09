package dev.antigravity.audiosync;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.app.usage.UsageEvents;
import android.app.usage.UsageStats;
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
import java.util.List;

public class AudioSyncService extends Service {
    private static final String TAG = "TvAudioSyncService";
    private static final String CHANNEL_ID = "tv_audio_sync_channel";
    private static final int NOTIFICATION_ID = 2026;

    public static final String ACTION_START = "ACTION_START";
    public static final String ACTION_STOP = "ACTION_STOP";
    public static final String EXTRA_RESULT_CODE = "EXTRA_RESULT_CODE";
    public static final String EXTRA_RESULT_DATA = "EXTRA_RESULT_DATA";
    public static final String EXTRA_IP = "EXTRA_IP";

    public static volatile boolean isRunning = false;
    public static volatile String currentIp = "";
    public static volatile boolean isHeadphonesConnected = false;
    public static volatile int globalDelayOffsetMs = 0;

    private volatile int currentTvVolume = 8;
    private volatile int currentStationVolume = 4;
    private volatile boolean currentIsMuted = false;

    // Dynamic delay calibration:
    // Tunneled / AV-Sync apps (YouTube, KinoPub, 4K video) -> 160 ms (calibrated with Dolby MS12)
    // Low-latency / live apps (Twitch AAudio)              -> 80 ms
    private volatile int currentTargetDelayMs = 160;

    private MediaProjection mediaProjection;
    private AudioRecord audioRecord;
    private Thread workerThread;
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

    private String currentForegroundPkg = "";
    private long lastEventsQueryTime = 0;

    private int detectOptimalDelay() {
        int baseDelay = 160; // Default sweet spot for YouTube, Kinopub, and standard media players (calibrated with Dolby MS12)

        if (usageStatsManager != null) {
            try {
                long now = System.currentTimeMillis();
                if (lastEventsQueryTime == 0) {
                    lastEventsQueryTime = now - 1000L * 3600 * 6; // Initial lookback: 6 hours
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
                lastEventsQueryTime = now - 5000L; // Overlap by 5s

                if (currentForegroundPkg.contains("twitch")) {
                    baseDelay = 80; // Twitch live stream low-latency mode (80 ms)
                } else if (currentForegroundPkg.contains("watch.kino") || currentForegroundPkg.contains("kinopub")) {
                    baseDelay = 150; // Kinopub multichannel 5.1 stream mode (150 ms)
                } else if (currentForegroundPkg.contains("spotify")) {
                    baseDelay = 150; // Spotify audio streaming mode (150 ms)
                } else {
                    baseDelay = 160; // YouTube / other media players (160 ms)
                }
            } catch (Exception e) {
                Log.w(TAG, "detectOptimalDelay error: " + e.getMessage());
            }
        }

        int effectiveDelay = baseDelay + globalDelayOffsetMs;
        if (effectiveDelay < 40) effectiveDelay = 40;
        if (effectiveDelay > 1200) effectiveDelay = 1200;
        return effectiveDelay;
    }

    @Override
    public void onCreate() {
        super.onCreate();
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
        currentTargetDelayMs = detectOptimalDelay();

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
                : "Трансляция звука на Яндекс Станцию активна";

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
                ip = prefs.getString("station_ip", "192.168.1.100");
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
                            .setPerformanceMode(AudioTrack.PERFORMANCE_MODE_LOW_LATENCY)
                            .build();

                    keepAliveTrack.setVolume(0.0f); // 100% silent, keeps HAL & PatchTrack alive
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
                            Log.w(TAG, "keepAliveTrack write error: " + written);
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
        startKeepAliveTrack(); // Pin MediaTek Audio HAL active to eliminate phase jumps
        Log.i(TAG, "AudioPlaybackCapture started. Target UDP: " + targetIp + ":12345");

        workerThread = new Thread(new Runnable() {
            @Override
            public void run() {
                DatagramSocket socket = null;
                try {
                    socket = new DatagramSocket();
                    InetAddress dest = InetAddress.getByName(targetIp);
                    int port = 12345;

                    byte[] pcmBuffer = new byte[1920];
                    byte[] sendBuffer = new byte[16 + 1920];
                    ByteBuffer bb = ByteBuffer.wrap(sendBuffer);
                    bb.order(ByteOrder.BIG_ENDIAN);

                    long packetCount = 0;
                    long lastLogTime = System.currentTimeMillis();
                    PowerManager pm = (PowerManager) getSystemService(Context.POWER_SERVICE);
                    updateTvVolume();
                    int loopCounter = 0;

                    while (isCapturing) {
                        if (audioRecord == null) {
                            SystemClock.sleep(50);
                            continue;
                        }

                        int read = audioRecord.read(pcmBuffer, 0, pcmBuffer.length);
                        if (read > 0) {
                            boolean isInteractive = pm == null || pm.isInteractive();
                            if (isScreenOff || !isInteractive || isHeadphonesConnected) {
                                continue;
                            }

                            loopCounter++;
                            if (loopCounter % 100 == 0) { // Every 1 second, detect active app profile
                                int detected = detectOptimalDelay();
                                if (detected != currentTargetDelayMs) {
                                    Log.i(TAG, "App profile switch: target delay changed " + currentTargetDelayMs + " ms -> " + detected + " ms");
                                    currentTargetDelayMs = detected;
                                }
                            }

                            // Pack 16-byte SYNC header + PCM data:
                            // 0..3: 'SYNC' (4B)
                            // 4..7: seq packetCount (4B)
                            // 8..11: timestamp ms (4B)
                            // 12: tvVolume (1B)
                            // 13: stationVolume (1B)
                            // 14: isMuted (1B)
                            // 15: targetDelay / 10 (1B, e.g. 13 for 130ms, 21 for 210ms)
                            bb.clear();
                            bb.putInt(0x53594E43); // 'SYNC' magic
                            bb.putInt((int) packetCount);
                            bb.putInt((int) (SystemClock.elapsedRealtime() & 0xFFFFFFFFL));
                            bb.put((byte) currentTvVolume);
                            bb.put((byte) currentStationVolume);
                            bb.put((byte) (currentIsMuted ? 1 : 0));
                            bb.put((byte) (currentTargetDelayMs / 10)); // Dynamic Target Delay from TV!
                            bb.put(pcmBuffer, 0, read);

                            DatagramPacket packet = new DatagramPacket(sendBuffer, 16 + read, dest, port);
                            socket.send(packet);
                            packetCount++;

                            long now = System.currentTimeMillis();
                            if (now - lastLogTime >= 5000) {
                                Log.i(TAG, String.format("Stream stats: %d packets sent (Active Hot Channel, targetDelay=%dms)", packetCount, currentTargetDelayMs));
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
                                    Log.e(TAG, "AudioRecord recovery failed (not initialized)");
                                    audioRecord = null;
                                    SystemClock.sleep(1000);
                                }
                            } catch (Exception recEx) {
                                Log.e(TAG, "AudioRecord recovery exception: " + recEx.getMessage());
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
        isCapturing = false;
        isRunning = false;
        currentIp = "";

        stopKeepAliveTrack();

        if (workerThread != null) {
            workerThread.interrupt();
            workerThread = null;
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
}
