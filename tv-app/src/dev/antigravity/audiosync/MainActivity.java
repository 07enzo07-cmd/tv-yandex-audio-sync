package dev.antigravity.audiosync;

import android.app.Activity;
import android.content.Context;
import android.content.Intent;
import android.media.projection.MediaProjectionManager;
import android.os.Bundle;
import android.view.View;
import android.widget.Button;
import android.widget.EditText;
import android.widget.TextView;
import android.widget.Toast;

public class MainActivity extends Activity {
    private static final int REQUEST_CODE_CAPTURE = 1001;

    private TextView tvStatus;
    private EditText etIp;
    private Button btnToggle;
    private boolean isAutoStart = false;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_main);

        tvStatus = findViewById(R.id.tvStatus);
        etIp = findViewById(R.id.etIp);
        btnToggle = findViewById(R.id.btnToggle);

        android.content.SharedPreferences prefs = getSharedPreferences("audiosync", Context.MODE_PRIVATE);
        String savedIp = prefs.getString("station_ip", "192.168.1.100");
        etIp.setText(savedIp);

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
                    startCapturePermission();
                }
            }
        });

        // If not already running, auto-trigger capture
        if (!AudioSyncService.isRunning) {
            startCapturePermission();
        } else if (isAutoStart) {
            finish();
        }
    }

    @Override
    protected void onResume() {
        super.onResume();
        updateUI();
    }

    private void updateUI() {
        if (AudioSyncService.isRunning) {
            if (AudioSyncService.isHeadphonesConnected) {
                tvStatus.setText("Статус: Наушники подключены (колонка на паузе)");
                tvStatus.setTextColor(0xFF00B0FF); // Light blue
            } else {
                tvStatus.setText("Статус: Трансляция активна -> " + AudioSyncService.currentIp);
                tvStatus.setTextColor(0xFF4CAF50); // Green
            }
            btnToggle.setText("ВЫКЛЮЧИТЬ СИНХРОНИЗАЦИЮ");
            btnToggle.setBackgroundColor(0xFFE53935); // Red
        } else {
            tvStatus.setText("Статус: Остановлено");
            tvStatus.setTextColor(0xFF888888);
            btnToggle.setText("ВКЛЮЧИТЬ СИНХРОНИЗАЦИЮ");
            btnToggle.setBackgroundColor(0xFF1E88E5); // Blue
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
                updateUI();
                if (isAutoStart) {
                    finish();
                }
            } else {
                Toast.makeText(this, "Разрешение на захват звука отклонено", Toast.LENGTH_LONG).show();
            }
        }
    }
}
