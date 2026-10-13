package com.recordingmcp.recorder.service

import android.app.Notification
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.IBinder
import androidx.core.app.ServiceCompat
import com.recordingmcp.recorder.Graph
import com.recordingmcp.recorder.R
import com.recordingmcp.recorder.audio.AudioCapture
import com.recordingmcp.recorder.audio.PcmChunkWriter
import com.recordingmcp.recorder.data.db.ChunkEntity
import com.recordingmcp.recorder.data.db.ChunkState
import com.recordingmcp.recorder.data.db.ChunkStateMachine
import com.recordingmcp.recorder.data.db.RecordingEntity
import com.recordingmcp.recorder.data.db.RecordingState
import com.recordingmcp.recorder.sync.UploadScheduler
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import java.io.File
import java.util.UUID
import android.Manifest
import android.content.pm.PackageManager
import androidx.core.content.ContextCompat
import com.recordingmcp.recorder.CHANNEL_RECORDING

/**
 * 前台麦克风服务（foregroundServiceType="microphone"）。
 * - 只从用户在前台界面点击"开始"后启动（不做 BOOT_COMPLETED 自动重启，系统也不允许后台启动 mic FGS）；
 * - START_NOT_STICKY；
 * - 每块落盘提交：Room 行 PERSISTED → UPLOAD_PENDING，并 enqueue UploadWorker；
 * - 停止时更新录音汇总并 enqueue FinishWorker。
 */
class RecordingService : Service() {

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    // 单线程持久化作用域：保证分块行按 seq 顺序写入。
    @OptIn(ExperimentalCoroutinesApi::class)
    private val persistScope = CoroutineScope(SupervisorJob() + Dispatchers.IO.limitedParallelism(1))

    private var capture: AudioCapture? = null
    private var writer: PcmChunkWriter? = null

    @Volatile
    private var activeRecordingId: String? = null

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            stopRecording()
            return START_NOT_STICKY
        }
        startRecording()
        return START_NOT_STICKY
    }

    private fun hasRecordPermission(): Boolean =
        ContextCompat.checkSelfPermission(this, Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED

    private fun startRecording() {
        if (activeRecordingId != null) return // 已在录音
        if (!hasRecordPermission()) {
            stopSelf()
            return
        }
        val graph = Graph.get(applicationContext)
        val recordingId = UUID.randomUUID().toString()
        activeRecordingId = recordingId

        // 前台通知必须立即发布（Android 12+ startForegroundService 限时要求）。
        ServiceCompat.startForeground(
            this,
            NOTIFICATION_ID,
            buildNotification(0),
            ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE,
        )

        val now = System.currentTimeMillis()
        val dir = File(graph.audioRoot, recordingId)
        activeStartedAt = now

        // persistScope 单线程：插入 → 分块行 → 停止汇总保持全局有序。
        persistScope.launch {
            // 崩溃恢复：上一进程遗留的 RECORDING 行（服务已不在）按已落盘分块收尾。
            finalizeStaleRecording()
            graph.db.recordingDao().insert(
                RecordingEntity(
                    recordingId = recordingId,
                    codec = "pcm_s16le",
                    sampleRate = AudioCapture.SAMPLE_RATE,
                    channels = 1,
                    startedAtEpochMs = now,
                    state = RecordingState.RECORDING.name,
                    totalChunks = null,
                    totalSamples = null,
                    durationMs = null,
                    createdAt = now,
                ),
            )
        }

        val newCapture = AudioCapture()
        val newWriter = PcmChunkWriter(dir, chunkSeconds = graph.settings.chunkSeconds())
        newWriter.onChunkCommitted = { committed -> persistCommittedChunk(recordingId, committed) }
        try {
            newCapture.start()
            newWriter.start()
        } catch (e: Exception) {
            runCatching { newCapture.stop() }
            activeRecordingId = null
            stopSelf()
            return
        }
        capture = newCapture
        writer = newWriter

        // 采集循环：每次读 3200 B（100 ms）。
        scope.launch {
            val buffer = ByteArray(AudioCapture.READ_BYTES)
            val cap = capture ?: return@launch
            val wr = writer ?: return@launch
            while (isActive && cap.isCapturing) {
                val n = cap.read(buffer)
                if (n > 0) {
                    try {
                        wr.write(buffer, 0, n)
                    } catch (e: Exception) {
                        break
                    }
                } else if (n < 0) {
                    break
                }
            }
        }

        // 通知计时。
        scope.launch {
            while (isActive && activeRecordingId != null) {
                val startedAt = activeStartedAt
                if (startedAt != null) {
                    updateNotification(buildNotification(System.currentTimeMillis() - startedAt))
                }
                delay(1000)
            }
        }
    }

    private fun persistCommittedChunk(recordingId: String, committed: com.recordingmcp.recorder.audio.CommittedChunk) {
        persistScope.launch {
            val graph = Graph.get(applicationContext)
            val dao = graph.db.chunkDao()
            val now = System.currentTimeMillis()
            dao.insert(
                ChunkEntity(
                    recordingId = recordingId,
                    seq = committed.seq,
                    sampleOffset = committed.sampleOffset,
                    sampleCount = committed.sampleCount,
                    bytes = committed.bytes,
                    sha256 = committed.sha256,
                    filePath = committed.file.absolutePath,
                    state = ChunkState.PERSISTED.name,
                    attempts = 0,
                    lastError = null,
                    updatedAt = now,
                ),
            )
            dao.updateState(recordingId, committed.seq, ChunkStateMachine.onPersisted().name, now)
            UploadScheduler.enqueueChunk(applicationContext, recordingId, committed.seq)
        }
    }

    private fun stopRecording() {
        val recordingId = activeRecordingId
        activeRecordingId = null
        val cap = capture
        capture = null
        val wr = writer
        writer = null

        persistScope.launch {
            val graph = Graph.get(applicationContext)
            cap?.stop()
            var summary: com.recordingmcp.recorder.audio.StopSummary? = null
            try {
                summary = wr?.stop()
            } catch (e: Exception) {
                // 写入器异常：已有分块照常保留，尾块交给 Reconcile 清理。
            }
            // 目标录音：本进程实例的活跃录音；否则（服务被系统重建后收到 STOP）取遗留的 RECORDING 行。
            val targetId = recordingId
                ?: graph.db.recordingDao().getActive()?.recordingId
                ?: return@launch
            summary?.committed?.forEach { committed ->
                persistCommittedChunkSync(targetId, committed)
            }
            val totalChunks: Int
            val totalSamples: Long
            if (summary != null) {
                totalChunks = summary.committedCount
                totalSamples = summary.totalSamples
            } else {
                // 进程已换代的崩溃恢复：以已落盘分块行如实汇总。
                val rows = graph.db.chunkDao().listForRecording(targetId)
                totalChunks = rows.size
                totalSamples = rows.sumOf { it.sampleCount.toLong() }
            }
            val durationMs = totalSamples * 1000 / AudioCapture.SAMPLE_RATE
            graph.db.recordingDao().updateFinished(
                targetId,
                RecordingState.COMPLETE.name,
                totalChunks,
                totalSamples,
                durationMs,
            )
            UploadScheduler.enqueueFinish(applicationContext, targetId)
            ServiceCompat.stopForeground(this@RecordingService, ServiceCompat.STOP_FOREGROUND_REMOVE)
            stopSelf()
        }
    }

    /** 上一进程崩溃遗留的 RECORDING 行：按已落盘分块如实置为 COMPLETE（尾块已随进程死亡丢弃）。 */
    private suspend fun finalizeStaleRecording() {
        if (activeRecordingId != null) return // 本实例正在录音，无遗留
        val graph = Graph.get(applicationContext)
        val stale = graph.db.recordingDao().getActive() ?: return
        val rows = graph.db.chunkDao().listForRecording(stale.recordingId)
        val rowSamples = rows.sumOf { it.sampleCount.toLong() }
        graph.db.recordingDao().updateFinished(
            stale.recordingId,
            RecordingState.COMPLETE.name,
            stale.totalChunks ?: rows.size,
            stale.totalSamples ?: rowSamples,
            stale.durationMs ?: (rowSamples * 1000 / AudioCapture.SAMPLE_RATE),
        )
    }

    /** 停止路径的尾块持久化（与采集期回调同一存储语义，但同步等待完成）。 */
    private suspend fun persistCommittedChunkSync(recordingId: String, committed: com.recordingmcp.recorder.audio.CommittedChunk) {
        val graph = Graph.get(applicationContext)
        val dao = graph.db.chunkDao()
        val now = System.currentTimeMillis()
        dao.insert(
            ChunkEntity(
                recordingId = recordingId,
                seq = committed.seq,
                sampleOffset = committed.sampleOffset,
                sampleCount = committed.sampleCount,
                bytes = committed.bytes,
                sha256 = committed.sha256,
                filePath = committed.file.absolutePath,
                state = ChunkState.PERSISTED.name,
                attempts = 0,
                lastError = null,
                updatedAt = now,
            ),
        )
        dao.updateState(recordingId, committed.seq, ChunkStateMachine.onPersisted().name, now)
        UploadScheduler.enqueueChunk(applicationContext, recordingId, committed.seq)
    }

    override fun onDestroy() {
        // 系统杀服务：停止采集；写入器交给 Reconcile 的崩溃恢复语义。
        capture?.stop()
        capture = null
        scope.cancel()
        persistScope.cancel()
        super.onDestroy()
    }

    @Volatile
    private var activeStartedAt: Long? = null

    private fun buildNotification(elapsedMs: Long): Notification {
        val stopIntent = PendingIntent.getService(
            this,
            0,
            Intent(this, RecordingService::class.java).setAction(ACTION_STOP),
            PendingIntent.FLAG_IMMUTABLE,
        )
        val totalSeconds = elapsedMs / 1000
        return Notification.Builder(this, CHANNEL_RECORDING)
            .setSmallIcon(R.drawable.ic_mic)
            .setContentTitle(getString(R.string.notif_recording_title))
            .setContentText(getString(R.string.notif_recording_elapsed, totalSeconds / 60, totalSeconds % 60))
            .setOngoing(true)
            .addAction(
                Notification.Action.Builder(
                    android.graphics.drawable.Icon.createWithResource(this, R.drawable.ic_mic),
                    getString(R.string.notif_stop),
                    stopIntent,
                ).build(),
            )
            .build()
    }

    private fun updateNotification(notification: Notification) {
        val manager = getSystemService(Context.NOTIFICATION_SERVICE) as android.app.NotificationManager
        manager.notify(NOTIFICATION_ID, notification)
    }

    companion object {
        const val ACTION_STOP = "com.recordingmcp.recorder.action.STOP"
        const val NOTIFICATION_ID = 1

        fun start(context: Context) {
            context.startForegroundService(Intent(context, RecordingService::class.java))
        }

        fun stop(context: Context) {
            context.startService(Intent(context, RecordingService::class.java).setAction(ACTION_STOP))
        }
    }
}
