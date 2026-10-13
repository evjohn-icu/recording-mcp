package com.recordingmcp.recorder.data.settings

import android.content.Context
import android.content.SharedPreferences
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.flow.distinctUntilChanged

/**
 * SharedPreferences 封装：UI 订阅 Flow，Worker/调度器用阻塞 getter。
 * 设备凭据仅存于应用私有目录（自托管单用户场景；可升级为 EncryptedSharedPreferences）。
 */
class SettingsStore(context: Context) {

    private val prefs: SharedPreferences =
        context.applicationContext.getSharedPreferences(FILE_NAME, Context.MODE_PRIVATE)

    // ---- Flows（UI 用） ----

    val serverUrlFlow: Flow<String> = stringFlow(KEY_SERVER_URL, DEFAULT_SERVER_URL)
    val deviceIdFlow: Flow<String?> = nullableStringFlow(KEY_DEVICE_ID)
    val deviceTokenFlow: Flow<String?> = nullableStringFlow(KEY_DEVICE_TOKEN)
    val chunkSecondsFlow: Flow<Int> = intFlow(KEY_CHUNK_SECONDS, DEFAULT_CHUNK_SECONDS)
    val wifiOnlyFlow: Flow<Boolean> = booleanFlow(KEY_WIFI_ONLY, false)
    val deleteAfterAckFlow: Flow<Boolean> = booleanFlow(KEY_DELETE_AFTER_ACK, false)

    // ---- 阻塞 getter（Worker/调度器用） ----

    fun serverUrl(): String = prefs.getString(KEY_SERVER_URL, DEFAULT_SERVER_URL) ?: DEFAULT_SERVER_URL

    fun deviceId(): String? = prefs.getString(KEY_DEVICE_ID, null)

    fun deviceToken(): String? = prefs.getString(KEY_DEVICE_TOKEN, null)

    /** 块时长（秒），读取时同样钳制到 15..60。 */
    fun chunkSeconds(): Int = (prefs.getInt(KEY_CHUNK_SECONDS, DEFAULT_CHUNK_SECONDS)).coerceIn(MIN_CHUNK_SECONDS, MAX_CHUNK_SECONDS)

    fun wifiOnly(): Boolean = prefs.getBoolean(KEY_WIFI_ONLY, false)

    fun deleteAfterAck(): Boolean = prefs.getBoolean(KEY_DELETE_AFTER_ACK, false)

    fun hasCredentials(): Boolean = !deviceId().isNullOrBlank() && !deviceToken().isNullOrBlank()

    // ---- 写入 ----

    fun setServerUrl(value: String) {
        prefs.edit().putString(KEY_SERVER_URL, value.trim()).apply()
    }

    fun setDeviceCredentials(deviceId: String, deviceToken: String) {
        prefs.edit()
            .putString(KEY_DEVICE_ID, deviceId)
            .putString(KEY_DEVICE_TOKEN, deviceToken)
            .apply()
    }

    fun clearDeviceCredentials() {
        prefs.edit().remove(KEY_DEVICE_ID).remove(KEY_DEVICE_TOKEN).apply()
    }

    fun setChunkSeconds(seconds: Int) {
        prefs.edit().putInt(KEY_CHUNK_SECONDS, seconds.coerceIn(MIN_CHUNK_SECONDS, MAX_CHUNK_SECONDS)).apply()
    }

    fun setWifiOnly(value: Boolean) {
        prefs.edit().putBoolean(KEY_WIFI_ONLY, value).apply()
    }

    fun setDeleteAfterAck(value: Boolean) {
        prefs.edit().putBoolean(KEY_DELETE_AFTER_ACK, value).apply()
    }

    // ---- 内部 ----

    private fun stringFlow(key: String, default: String): Flow<String> = callbackFlow {
        val listener = SharedPreferences.OnSharedPreferenceChangeListener { _, changedKey ->
            if (changedKey == key) trySend(prefs.getString(key, default) ?: default)
        }
        prefs.registerOnSharedPreferenceChangeListener(listener)
        trySend(prefs.getString(key, default) ?: default)
        awaitClose { prefs.unregisterOnSharedPreferenceChangeListener(listener) }
    }.distinctUntilChanged()

    private fun nullableStringFlow(key: String): Flow<String?> = callbackFlow {
        val listener = SharedPreferences.OnSharedPreferenceChangeListener { _, changedKey ->
            if (changedKey == key) trySend(prefs.getString(key, null))
        }
        prefs.registerOnSharedPreferenceChangeListener(listener)
        trySend(prefs.getString(key, null))
        awaitClose { prefs.unregisterOnSharedPreferenceChangeListener(listener) }
    }.distinctUntilChanged()

    private fun intFlow(key: String, default: Int): Flow<Int> = callbackFlow {
        val listener = SharedPreferences.OnSharedPreferenceChangeListener { _, changedKey ->
            if (changedKey == key) trySend(prefs.getInt(key, default))
        }
        prefs.registerOnSharedPreferenceChangeListener(listener)
        trySend(prefs.getInt(key, default))
        awaitClose { prefs.unregisterOnSharedPreferenceChangeListener(listener) }
    }.distinctUntilChanged()

    private fun booleanFlow(key: String, default: Boolean): Flow<Boolean> = callbackFlow {
        val listener = SharedPreferences.OnSharedPreferenceChangeListener { _, changedKey ->
            if (changedKey == key) trySend(prefs.getBoolean(key, default))
        }
        prefs.registerOnSharedPreferenceChangeListener(listener)
        trySend(prefs.getBoolean(key, default))
        awaitClose { prefs.unregisterOnSharedPreferenceChangeListener(listener) }
    }.distinctUntilChanged()

    companion object {
        private const val FILE_NAME = "recorder_settings"
        private const val KEY_SERVER_URL = "server_url"
        private const val KEY_DEVICE_ID = "device_id"
        private const val KEY_DEVICE_TOKEN = "device_token"
        private const val KEY_CHUNK_SECONDS = "chunk_seconds"
        private const val KEY_WIFI_ONLY = "wifi_only"
        private const val KEY_DELETE_AFTER_ACK = "delete_after_ack"

        /** 默认指向宿主机（Android 模拟器 10.0.2.2 = host loopback），适配本地 wrangler dev。 */
        const val DEFAULT_SERVER_URL = "http://10.0.2.2:8787"
        const val DEFAULT_CHUNK_SECONDS = 30
        const val MIN_CHUNK_SECONDS = 15
        const val MAX_CHUNK_SECONDS = 60
    }
}
