package com.recordingmcp.recorder.sync

import android.content.Context
import androidx.work.BackoffPolicy
import androidx.work.Constraints
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.ExistingWorkPolicy
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.workDataOf
import com.recordingmcp.recorder.data.settings.SettingsStore
import java.time.Duration

/** WorkManager 入队封装：唯一名 + 约束 + 指数退避。 */
object UploadScheduler {

    const val KEY_RECORDING_ID = "recordingId"
    const val KEY_SEQ = "seq"

    private fun constraints(wifiOnly: Boolean): Constraints = Constraints.Builder()
        .setRequiredNetworkType(if (wifiOnly) NetworkType.UNMETERED else NetworkType.CONNECTED)
        .build()

    /** 单块上传：唯一名 upload-<recordingId>-<seq>，KEEP 防重复。 */
    fun enqueueChunk(context: Context, recordingId: String, seq: Int) {
        val settings = SettingsStore(context)
        val request = OneTimeWorkRequestBuilder<UploadWorker>()
            .setInputData(workDataOf(KEY_RECORDING_ID to recordingId, KEY_SEQ to seq))
            .setConstraints(constraints(settings.wifiOnly()))
            .setBackoffCriteria(BackoffPolicy.EXPONENTIAL, Duration.ofSeconds(30))
            .build()
        WorkManager.getInstance(context)
            .enqueueUniqueWork("upload-$recordingId-$seq", ExistingWorkPolicy.KEEP, request)
    }

    /** 结束录音：唯一名 finish-<recordingId>。 */
    fun enqueueFinish(context: Context, recordingId: String) {
        val settings = SettingsStore(context)
        val request = OneTimeWorkRequestBuilder<FinishWorker>()
            .setInputData(workDataOf(KEY_RECORDING_ID to recordingId))
            .setConstraints(constraints(settings.wifiOnly()))
            .setBackoffCriteria(BackoffPolicy.EXPONENTIAL, Duration.ofSeconds(30))
            .build()
        WorkManager.getInstance(context)
            .enqueueUniqueWork("finish-$recordingId", ExistingWorkPolicy.KEEP, request)
    }

    /** 立即跑一轮对账（删除确认等需要及时性的操作）。 */
    fun enqueueReconcileOnce(context: Context) {
        val request = OneTimeWorkRequestBuilder<ReconcileWorker>()
            .setConstraints(constraints(SettingsStore(context).wifiOnly()))
            .build()
        WorkManager.getInstance(context)
            .enqueueUniqueWork("reconcile-once", ExistingWorkPolicy.KEEP, request)
    }

    /** 周期对账（15 分钟，唯一名 reconcile），Application 启动时注册。 */
    fun schedulePeriodicReconcile(context: Context) {
        val request = PeriodicWorkRequestBuilder<ReconcileWorker>(Duration.ofMinutes(15))
            .setConstraints(constraints(SettingsStore(context).wifiOnly()))
            .build()
        WorkManager.getInstance(context)
            .enqueueUniquePeriodicWork("reconcile", ExistingPeriodicWorkPolicy.KEEP, request)
    }
}
