package dev.antigravity.audiosync;

import android.app.Application;
import android.content.Context;
import android.database.ContentObserver;
import android.os.Handler;
import android.os.Looper;
import android.provider.Settings;
import android.util.Log;

public final class DspPreferences extends Application {
    private static final String[] KEYS = {
        "sound_advanced_surround_virtualizer",
        "sound_advanced_dolby_atmos",
        "sound_bass_enhancement"
    };

    static boolean isProtected(String key) {
        for (String setting : KEYS) if (setting.equals(key)) return true;
        return false;
    }

    static void choose(Context context, String key, int value) {
        context.getSharedPreferences("dsp_owner", MODE_PRIVATE).edit().putInt(key, value).apply();
        reconcile(context, key);
    }

    private static void reconcile(Context context, String key) {
        int desired = context.getSharedPreferences("dsp_owner", MODE_PRIVATE).getInt(key, 1);
        if (Settings.Global.getInt(context.getContentResolver(), key, -1) == desired) return;
        try {
            if (!Settings.Global.putInt(context.getContentResolver(), key, desired)) {
                Log.w("TvAudioDSP", "Write rejected: " + key);
            }
        } catch (SecurityException error) {
            Log.w("TvAudioDSP", "Missing setting permission: " + key, error);
        }
    }

    @Override public void onCreate() {
        super.onCreate();
        Handler main = new Handler(Looper.getMainLooper());
        for (final String key : KEYS) {
            getContentResolver().registerContentObserver(Settings.Global.getUriFor(key), false,
                    new ContentObserver(main) {
                        @Override public void onChange(boolean selfChange) {
                            reconcile(DspPreferences.this, key);
                        }
                    });
            reconcile(this, key);
        }
    }
}
