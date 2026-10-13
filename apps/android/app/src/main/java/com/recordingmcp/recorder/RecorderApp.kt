package com.recordingmcp.recorder

import android.app.Application
import android.app.NotificationChannel
import android.app.NotificationManager
import com.recordingmcp.recorder.data.db.AppDatabase
import com.recordingmcp.recorder.data.remote.IngestApi
import com.recordingmcp.recorder.data.settings.SettingsStore
import com.recordingmcp.recorder.sync.UploadScheduler
import java.io.File

/** 通知渠道 id：录音前台服务专用（plan 字面量 "recording"）。 */
const val CHANNEL_RECORDING = "recording"

/** 简单服务定位器：无 DI 框架，Application 启动时初始化，Worker/Service/Compose 共享。 */
class Graph private constructor(context: android.content.Context) {

    val appContext: android.content.Context = context.applicationContext

    val db: AppDatabase by lazy { AppDatabase.build(appContext) }

    val settings: SettingsStore by lazy { SettingsStore(appContext) }

    /** 录音根目录：filesDir/recordings/<recordingId>/chunk-NNNNN.pcm */
    val audioRoot: File get() = File(appContext.filesDir, "recordings")

    /** 每次 API 访问重建（serverUrl/token 可能在设置页变更）。 */
    fun api(): IngestApi = IngestApi(
        baseUrl = settings.serverUrl(),
        tokenProvider = { settings.deviceToken() },
    )

    companion object {
        @Volatile
        private var instance: Graph? = null

        fun init(context: android.content.Context) {
            if (instance == null) {
                synchronized(this) {
                    if (instance == null) instance = Graph(context)
                }
            }
        }

        fun get(context: android.content.Context): Graph {
            instance?.let { return it }
            init(context)
            return instance!!
        }
    }
}

class RecorderApp : Application() {

    override fun onCreate() {
        super.onCreate()
        Graph.init(this)
        val manager = getSystemService(NotificationManager::class.java)
        manager.createNotificationChannel(
            NotificationChannel(
                CHANNEL_RECORDING,
                getString(R.string.channel_recording),
                NotificationManager.IMPORTANCE_LOW,
            ).apply {
                description = "录音前台服务通知"
            },
        )
        // 周期对账：孤儿清理、重新入队、完成确认、云端状态回填。
        UploadScheduler.schedulePeriodicReconcile(this)
    }
}
