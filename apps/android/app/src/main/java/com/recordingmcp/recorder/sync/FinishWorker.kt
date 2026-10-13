package com.recordingmcp.recorder.sync

import android.content.Context
import androidx.work.CoroutineWorker
import androidx.work.WorkerParameters
import com.recordingmcp.recorder.Graph
import com.recordingmcp.recorder.data.db.ChunkState
import com.recordingmcp.recorder.data.db.RecordingState
import com.recordingmcp.recorder.data.remote.ApiError
import com.recordingmcp.recorder.data.remote.FinishRequest
import java.io.IOException

/**
 * 结束录音 Worker（唯一名 finish-<recordingId>）：
 * - 200 → 本地记录 COMPLETE；
 * - 409 sequence_gap → 按 details.missing_sequences 把缺失块重新置为 UPLOAD_PENDING 并入队，然后 retry；
 * - 永久码（conflict_payload 等）→ failure；
 * - 其它（网络 / 5xx / 429）→ retry。
 * 缺失序列永远不得 finished —— 服务端契约保证，客户端只在全部块 CLOUD_PERSISTED 后才真正调用 finish。
 */
class FinishWorker(context: Context, params: WorkerParameters) : CoroutineWorker(context, params) {

    private val graph = Graph.get(applicationContext)

    override suspend fun doWork(): Result {
        val recordingId = inputData.getString(UploadScheduler.KEY_RECORDING_ID) ?: return Result.failure()
        val recordingDao = graph.db.recordingDao()
        val chunkDao = graph.db.chunkDao()

        val recording = recordingDao.get(recordingId) ?: return Result.failure()
        if (recording.state == RecordingState.DELETE_PENDING.name || recording.state == RecordingState.DELETED.name) {
            return Result.success()
        }

        val chunks = chunkDao.listForRecording(recordingId)
        if (chunks.isEmpty()) return Result.retry()

        val totalSamples = recording.totalSamples ?: chunks.sumOf { it.sampleCount.toLong() }
        val totalChunks = recording.totalChunks ?: chunks.size
        val durationMs = recording.durationMs ?: (totalSamples * 1000 / recording.sampleRate)

        val token = graph.settings.deviceToken()
        if (token.isNullOrBlank()) return Result.retry()

        val api = graph.api()
        return try {
            val response = api.finish(
                recordingId,
                FinishRequest(
                    totalChunks = totalChunks,
                    totalSamples = totalSamples,
                    totalDurationMs = durationMs,
                ),
            )
            if (!response.durable) throw IOException("finish ack not durable: state=${response.state}")
            recordingDao.updateState(recordingId, RecordingState.COMPLETE.name)
            Result.success()
        } catch (e: ApiError) {
            if (e.status == 409 && e.code == "sequence_gap") {
                val missing = e.missingSequences()
                // 缺失块：本地文件完好则重新入队上传（状态回到 UPLOAD_PENDING）。
                for (seq in missing) {
                    val chunk = chunkDao.get(recordingId, seq) ?: continue
                    if (chunk.state == ChunkState.CLOUD_PERSISTED.name) continue
                    if (!java.io.File(chunk.filePath).exists()) continue
                    chunkDao.updateState(recordingId, seq, ChunkState.UPLOAD_PENDING.name, System.currentTimeMillis())
                    UploadScheduler.enqueueChunk(applicationContext, recordingId, seq)
                }
                Result.retry()
            } else if (e.isPermanent) {
                Result.failure()
            } else {
                Result.retry()
            }
        } catch (e: IOException) {
            Result.retry()
        }
    }
}
