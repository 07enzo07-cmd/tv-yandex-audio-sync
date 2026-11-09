package dev.antigravity.audiosync;

import android.app.Activity;
import android.content.Context;
import android.content.Intent;
import android.media.projection.MediaProjectionManager;
import android.os.Bundle;
import android.provider.Settings;
import android.util.Log;
import android.view.View;
import android.widget.Button;
import android.widget.EditText;
import android.widget.TextView;
import android.widget.Toast;

public class MainActivity extends Activity {
    private static final String TAG = "MainActivity";
    private static final int REQUEST_CODE_CAPTURE = 1001;

    private TextView tvStatus;
    private EditText etIp;
    private Button btnToggle;
    private TextView tvDelayInfo;
    private Button btnDelayMinus;
    private Button btnDelayReset;
    private Button btnDelayPlus;
    private int currentOffset = 0;
    private boolean isAutoStart = false;

    private static class SoundModule {
        final int buttonId;
        final String settingKey;
        final String name;
        Button button;

        SoundModule(int buttonId, String settingKey, String name) {
            this.buttonId = buttonId;
            this.settingKey = settingKey;
            this.name = name;
        }
    }

    private final SoundModule[] soundModules = new SoundModule[] {
        new SoundModule(R.id.btnModDolbyAtmos, "sound_advanced_dolby_atmos", "Dolby Atmos"),
        new SoundModule(R.id.btnModSurroundVirt, "sound_advanced_surround_virtualizer", "Surround Virtualizer"),
        new SoundModule(R.id.btnModVolumeLeveler, "sound_advanced_volume_leveler", "Volume Leveler"),
        new SoundModule(R.id.btnModDialogEnhancer, "sound_advanced_dialogue_enhancer", "Dialogue Enhancer"),
        new SoundModule(R.id.btnModBassEnhance, "sound_bass_enhancement", "Bass Enhancement"),
        new SoundModule(R.id.btnModDtsVirtX, "sound_dts_virtual_x", "DTS Virtual:X"),
        new SoundModule(R.id.btnModDtsSurround, "sound_dts_studio_surround", "DTS Studio Surround"),
        new SoundModule(R.id.btnModDbxSurround, "sound_dbx_total_surround", "DBX Total Surround"),
        new SoundModule(R.id.btnModDbxSonic, "sound_dbx_total_sonic", "DBX Total Sonic"),
        new SoundModule(R.id.btnModSonicEmotion, "sound_sonic_emotion", "Sonic Emotion 3D")
    };

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_main);

        tvStatus = findViewById(R.id.tvStatus);
        etIp = findViewById(R.id.etIp);
        btnToggle = findViewById(R.id.btnToggle);
        tvDelayInfo = findViewById(R.id.tvDelayInfo);
        btnDelayMinus = findViewById(R.id.btnDelayMinus);
        btnDelayReset = findViewById(R.id.btnDelayReset);
        btnDelayPlus = findViewById(R.id.btnDelayPlus);

        final android.content.SharedPreferences prefs = getSharedPreferences("audiosync", Context.MODE_PRIVATE);
        String savedIp = prefs.getString("station_ip", "192.168.1.100");
        etIp.setText(savedIp);

        updateDelayInfo(prefs);

        btnDelayMinus.setOnClickListener(new View.OnClickListener() {
            @Override
            public void onClick(View v) {
                if (currentOffset > -100) {
                    currentOffset -= 10;
                    prefs.edit().putInt("delay_offset", currentOffset).apply();
                    updateDelayInfo(prefs);
                }
            }
        });

        btnDelayReset.setOnClickListener(new View.OnClickListener() {
            @Override
            public void onClick(View v) {
                currentOffset = 0;
                prefs.edit().putInt("delay_offset", currentOffset).apply();
                updateDelayInfo(prefs);
            }
        });

        btnDelayPlus.setOnClickListener(new View.OnClickListener() {
            @Override
            public void onClick(View v) {
                if (currentOffset < 100) {
                    currentOffset += 10;
                    prefs.edit().putInt("delay_offset", currentOffset).apply();
                    updateDelayInfo(prefs);
                }
            }
        });

        initSoundModuleViews();

        Button btnPresetAtmos = findViewById(R.id.btnPresetAtmos);
        if (btnPresetAtmos != null) {
            btnPresetAtmos.setOnClickListener(new View.OnClickListener() {
                @Override
                public void onClick(View v) {
                    setGlobalAudioSetting("sound_advanced_dolby_atmos", 1);
                    setGlobalAudioSetting("sound_advanced_surround_virtualizer", 1);
                    setGlobalAudioSetting("sound_bass_enhancement", 1);
                    updateAllSoundModuleButtons();
                    Toast.makeText(MainActivity.this, "Включен набор: Atmos + Virtualizer + Bass", Toast.LENGTH_SHORT).show();
                }
            });
        }

        Button btnPresetReset = findViewById(R.id.btnPresetReset);
        if (btnPresetReset != null) {
            btnPresetReset.setOnClickListener(new View.OnClickListener() {
                @Override
                public void onClick(View v) {
                    for (SoundModule mod : soundModules) {
                        setGlobalAudioSetting(mod.settingKey, 0);
                    }
                    updateAllSoundModuleButtons();
                    Toast.makeText(MainActivity.this, "Все модули звука отключены", Toast.LENGTH_SHORT).show();
                }
            });
        }

        if (getIntent() != null) {
            isAutoStart = getIntent().getBooleanExtra("EXTRA_AUTO_START", false);
            String extraIp = getIntent().getStringExtra("EXTRA_IP");
            if (extraIp != null && !extraIp.isEmpty()) {
                savedIp = extraIp;
                etIp.setText(savedIp);
                prefs.edit().putString("station_ip", savedIp).apply();
            }
        }

        updateUI();

        btnToggle.setOnClickListener(new View.OnClickListener() {
            @Override
            public void onClick(View v) {
                if (AudioSyncService.isRunning) {
                    stopSyncService();
                } else {
                    btnToggle.setEnabled(false);
                    btnToggle.setText("ПОДКЛЮЧЕНИЕ...");
                    startCapturePermission();
                }
            }
        });

        // Only auto-trigger capture if launched via boot / auto-start
        if (isAutoStart && !AudioSyncService.isRunning) {
            startCapturePermission();
        } else if (isAutoStart && AudioSyncService.isRunning) {
            finish();
        }
    }

    private void initSoundModuleViews() {
        for (final SoundModule mod : soundModules) {
            mod.button = findViewById(mod.buttonId);
            if (mod.button != null) {
                updateModuleButtonState(mod);
                mod.button.setOnClickListener(new View.OnClickListener() {
                    @Override
                    public void onClick(View v) {
                        int currentVal = getGlobalAudioSetting(mod.settingKey, 0);
                        int newVal = currentVal == 1 ? 0 : 1;
                        setGlobalAudioSetting(mod.settingKey, newVal);
                        updateModuleButtonState(mod);
                        String stateStr = newVal == 1 ? "ВКЛЮЧЕН" : "ВЫКЛЮЧЕН";
                        Toast.makeText(MainActivity.this, mod.name + ": " + stateStr, Toast.LENGTH_SHORT).show();
                    }
                });
            }
        }
    }

    private void updateAllSoundModuleButtons() {
        for (SoundModule mod : soundModules) {
            updateModuleButtonState(mod);
        }
    }

    private void updateModuleButtonState(SoundModule mod) {
        if (mod.button == null) return;
        int val = getGlobalAudioSetting(mod.settingKey, 0);
        if (val == 1) {
            mod.button.setText("ВКЛ");
            mod.button.setBackgroundResource(R.drawable.btn_module_on);
            mod.button.setTextColor(0xFFFFFFFF);
        } else {
            mod.button.setText("ВЫКЛ");
            mod.button.setBackgroundResource(R.drawable.btn_module_off);
            mod.button.setTextColor(0xFFB0BEC5);
        }
    }

    private int getGlobalAudioSetting(String key, int def) {
        try {
            return Settings.Global.getInt(getContentResolver(), key, def);
        } catch (Throwable t) {
            return def;
        }
    }

    private void setGlobalAudioSetting(String key, int val) {
        try {
            Settings.Global.putInt(getContentResolver(), key, val);
            Log.i(TAG, "setGlobalAudioSetting: " + key + " = " + val + " (via Settings.Global)");
        } catch (Throwable t) {
            Log.w(TAG, "Settings.Global write failed (" + t.getMessage() + "), attempting runtime exec fallback");
            try {
                Runtime.getRuntime().exec(new String[]{"settings", "put", "global", key, String.valueOf(val)});
            } catch (Exception e) {
                Log.e(TAG, "Runtime exec fallback error: " + e.getMessage());
            }
        }
    }

    @Override
    protected void onResume() {
        super.onResume();
        final android.content.SharedPreferences prefs = getSharedPreferences("audiosync", Context.MODE_PRIVATE);
        updateDelayInfo(prefs);
        updateUI();
        updateAllSoundModuleButtons();
    }

    private void updateDelayInfo(android.content.SharedPreferences prefs) {
        currentOffset = prefs.getInt("delay_offset", 0);
        AudioSyncService.globalDelayOffsetMs = currentOffset;
        String sign = currentOffset > 0 ? "+" : "";
        if (tvDelayInfo != null) {
            tvDelayInfo.setText("Смещение: " + sign + currentOffset + " мс (YouTube: " + (160 + currentOffset) + " мс, Кинопаб: " + (480 + currentOffset) + " мс, Spotify: " + (800 + currentOffset) + " мс, Twitch: " + (70 + currentOffset) + " мс)");
        }
    }

    private void updateUI() {
        if (btnToggle == null) return;
        btnToggle.setEnabled(true);
        if (AudioSyncService.isRunning) {
            if (AudioSyncService.isHeadphonesConnected) {
                tvStatus.setText("Статус: Наушники подключены (колонка на паузе)");
                tvStatus.setTextColor(0xFF00B0FF); // Light blue
            } else {
                tvStatus.setText("Статус: Трансляция активна -> " + AudioSyncService.currentIp);
                tvStatus.setTextColor(0xFF4CAF50); // Green
            }
            btnToggle.setText("ВЫКЛЮЧИТЬ СИНХРОНИЗАЦИЮ");
            btnToggle.setBackgroundResource(R.drawable.btn_toggle_stop);
        } else {
            tvStatus.setText("Статус: Остановлено");
            tvStatus.setTextColor(0xFF888888);
            btnToggle.setText("ВКЛЮЧИТЬ СИНХРОНИЗАЦИЮ");
            btnToggle.setBackgroundResource(R.drawable.btn_toggle_start);
        }
    }

    private void startCapturePermission() {
        MediaProjectionManager manager = (MediaProjectionManager) getSystemService(Context.MEDIA_PROJECTION_SERVICE);
        if (manager != null) {
            startActivityForResult(manager.createScreenCaptureIntent(), REQUEST_CODE_CAPTURE);
        }
    }

    private void stopSyncService() {
        AudioSyncService.isRunning = false;
        Intent intent = new Intent(this, AudioSyncService.class);
        intent.setAction(AudioSyncService.ACTION_STOP);
        startService(intent);
        updateUI();
    }

    @Override
    protected void onActivityResult(int requestCode, int resultCode, Intent data) {
        if (requestCode == REQUEST_CODE_CAPTURE) {
            btnToggle.setEnabled(true);
            if (resultCode == Activity.RESULT_OK && data != null) {
                String ip = etIp.getText().toString().trim();
                if (ip.isEmpty()) ip = "192.168.1.100";
                getSharedPreferences("audiosync", Context.MODE_PRIVATE).edit().putString("station_ip", ip).apply();

                Intent serviceIntent = new Intent(this, AudioSyncService.class);
                serviceIntent.setAction(AudioSyncService.ACTION_START);
                serviceIntent.putExtra(AudioSyncService.EXTRA_RESULT_CODE, resultCode);
                serviceIntent.putExtra(AudioSyncService.EXTRA_RESULT_DATA, data);
                serviceIntent.putExtra(AudioSyncService.EXTRA_IP, ip);

                startForegroundService(serviceIntent);
                AudioSyncService.isRunning = true;
                AudioSyncService.currentIp = ip;
                updateUI();
                if (isAutoStart) {
                    finish();
                }
            } else {
                updateUI();
                Toast.makeText(this, "Разрешение на захват звука отклонено", Toast.LENGTH_LONG).show();
            }
        }
    }
}
