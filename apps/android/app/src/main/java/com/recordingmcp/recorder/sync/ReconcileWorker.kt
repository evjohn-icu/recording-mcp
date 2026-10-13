package com.recordingmcp.recorder.sync

import android.content.Context
import androidx.work.CoroutineWorker
import androidx.work.WorkerParameters
import com.recordingmcp.recorder.Graph
import com.recordingmcp.recorder.data.db.ChunkState
import com.recordingmcp.recorder.data.db.ChunkStateMachine
import com.recordingmcp.recorder.data.db.RecordingState
import com.recordingmcp.recorder.data.remote.ApiError
import com.recordingmcp.recorder.data.remote.parseRfc3339ToEpochMs
import kotlinx.coroutines.flow.first
import java.io.File
import java.io.IOException

/**
 * 周期对账（15 分钟，唯一名 reconcile）：
 * 1. 孤儿清理：无 Room 行的录音目录整体删除；存在录音行时清理残留 `.part`；
 * 2. 崩溃恢复：WRITING 行删除（块未持久化）；UPLOADING 行按文件存在与否恢复为 UPLOAD_PENDING / FAILED_PERMANENT；
 * 3. 重新入队 UPLOAD_PENDING / FAILED_RETRYABLE；
 * 4. DELETE_PENDING → 云端 DELETE（幂等）成功后清除本地文件与行；失败留待下轮；
 * 5. 轮询 getRecording 回填 syncedAt / transcriptionState；云端未 complete 的 COMPLETE 录音入队 FinishWorker；
 * 6. deleteAfterAck 且云端已转写完成（ingested/skipped）→ 清理本地音频文件。
 *
 * 已派生的记忆数据不受删除影响（审计边界由云端与 Core 侧定义）。
 */
class ReconcileWorker(context: Context, params: WorkerParameters) : CoroutineWorker(context, params) {

    private val graph = Graph.get(applicationContext)

    override suspend fun doWork(): Result {
        val recordingDao = graph.db.recordingDao()
        val chunkDao = graph.db.chunkDao()
        val settings = graph.settings
        val allRecordings = recordingDao.observeAll().first()
        val knownIds = allRecordings.map { it.recordingId }.toSet()

        // 1. 孤儿目录与 .part 清理
        cleanupOrphans(knownIds)

        // 2. 崩溃恢复
        for (row in chunkDao.listWriting()) {
            if (ChunkStateMachine.resolveAfterCrash(ChunkState.WRITING, fileExists = true) == null) {
                chunkDao.delete(row.recordingId, row.seq)
            }
        }
        for (row in chunkDao.listUploading()) {
            val fileExists = File(row.filePath).exists()
            val resolved = ChunkStateMachine.resolveAfterCrash(ChunkState.UPLOADING, fileExists)
            if (resolved == null) {
                chunkDao.delete(row.recordingId, row.seq)
            } else {
                chunkDao.updateState(row.recordingId, row.seq, resolved.name, System.currentTimeMillis())
            }
        }

        // 3. 重新入队可重试块
        for (row in chunkDao.listRequeueable()) {
            UploadScheduler.enqueueChunk(applicationContext, row.recordingId, row.seq)
        }

        val token = settings.deviceToken()
        if (token.isNullOrBlank()) return Result.success()

        val api = graph.api()
        for (recording in allRecordings) {
            when (recording.state) {
                RecordingState.DELETE_PENDING.name -> {
                    if (deleteRemote(api, recording.recordingId)) {
                        purgeLocal(recording.recordingId)
                    }
                }

                RecordingState.COMPLETE.name -> {
                    val detail = try {
                        api.getRecording(recording.recordingId)
                    } catch (e: ApiError) {
                        null // 404 等：下轮再看
                    } catch (e: IOException) {
                        null
                    }
                    if (detail != null) {
                        if (detail.state != "complete") {
                            // 云端未 finish（例如上传中断后服务重启）：重新走 FinishWorker。
                            UploadScheduler.enqueueFinish(applicationContext, recording.recordingId)
                        }
                        recordingDao.updateSyncInfo(
                            recording.recordingId,
                            parseRfc3339ToEpochMs(detail.sync?.syncedAt),
                            detail.sync?.transcriptionState,
                        )
                        val transcriptionState = detail.sync?.transcriptionState
                        if (settings.deleteAfterAck() && transcriptionState != null && transcriptionState in ACKED_TRANSCRIPTION_STATES) {
                            purgeAudioFiles(recording.recordingId)
                        }
                    }
                }
            }
        }

        // 6. deleteAfterAck（无网络时云端状态拿不到，保持文件，下轮重试）
        if (settings.deleteAfterAck()) {
            for (recording in recordingDao.observeAll().first()) {
                val ts = recording.transcriptionState
                if (recording.syncedAt != null && ts != null && ts in ACKED_TRANSCRIPTION_STATES) {
                    purgeAudioFiles(recording.recordingId)
                }
            }
        }

        return Result.success()
    }

    private fun cleanupOrphans(knownIds: Set<String>) {
        val root = graph.audioRoot
        if (!root.isDirectory) return
        val now = System.currentTimeMillis()
        root.listFiles()?.forEach { dir ->
            if (!dir.isDirectory) {
                if (now - dir.lastModified() > ORPHAN_MIN_AGE_MS) dir.delete()
                return@forEach
            }
            if (dir.name !in knownIds) {
                // 无 Room 行的录音目录：整目录删除（崩溃残留或已删除行）。
                if (now - dir.lastModified() > ORPHAN_MIN_AGE_MS) dir.deleteRecursively()
                return@forEach
            }
            // 有行但残留 .part（崩溃时正在写的块）：直接删除。
            dir.listFiles()?.filter { it.name.endsWith(".part") }?.forEach { it.delete() }
        }
    }

    private suspend fun deleteRemote(api: com.recordingmcp.recorder.data.remote.IngestApi, recordingId: String): Boolean =
        try {
            api.deleteRecording(recordingId)
            true
        } catch (e: ApiError) {
            // 404 recording_not_found 视为已删除；其它错误下轮重试。
            e.status == 404
        } catch (e: IOException) {
            false
        }

    private suspend fun purgeLocal(recordingId: String) {
        purgeAudioFiles(recordingId)
        graph.db.chunkDao().deleteForRecording(recordingId)
        graph.db.recordingDao().delete(recordingId)
    }

    private fun purgeAudioFiles(recordingId: String) {
        File(graph.audioRoot, recordingId).deleteRecursively()
    }

    companion object {
        /** 云端转写推进到这些状态即视为该录音已消费完毕。 */
        val ACKED_TRANSCRIPTION_STATES = setOf("ingested", "skipped")

        /** 孤儿文件最小年龄，避免误删正在写入的目录。 */
        const val ORPHAN_MIN_AGE_MS = 60 * 60 * 1000L
    }
}
