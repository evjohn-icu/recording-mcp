package com.recordingmcp.recorder.sync

import android.content.Context
import androidx.work.CoroutineWorker
import androidx.work.WorkerParameters
import com.recordingmcp.recorder.Graph
import com.recordingmcp.recorder.audio.toLowerHex
import com.recordingmcp.recorder.data.db.ChunkState
import com.recordingmcp.recorder.data.db.ChunkStateMachine
import com.recordingmcp.recorder.data.db.RecordingState
import com.recordingmcp.recorder.data.remote.ApiError
import com.recordingmcp.recorder.data.remote.ChunkMeta
import com.recordingmcp.recorder.data.remote.RecordingCreateRequest
import java.io.File
import java.io.IOException
import java.security.MessageDigest
import java.time.Instant
import java.time.ZoneOffset
import java.time.format.DateTimeFormatter

/**
 * 单块上传 Worker：
 * - 文件缺失 → FAILED_PERMANENT；
 * - 200（durable ACK）→ CLOUD_PERSISTED；
 * - ApiError 永久码（validation_failed / conflict_checksum / conflict_payload）→ FAILED_PERMANENT；
 * - 其余（网络 / 5xx / 429 / 其它 ApiError）→ Result.retry()。
 * 未配对（无 token）时 retry，等待用户完成配对。
 */
class UploadWorker(context: Context, params: WorkerParameters) : CoroutineWorker(context, params) {

    private val graph = Graph.get(applicationContext)

    override suspend fun doWork(): Result {
        val recordingId = inputData.getString(UploadScheduler.KEY_RECORDING_ID) ?: return Result.failure()
        val seq = inputData.getInt(UploadScheduler.KEY_SEQ, -1)
        if (seq < 0) return Result.failure()

        val chunkDao = graph.db.chunkDao()
        val recordingDao = graph.db.recordingDao()

        val chunk = chunkDao.get(recordingId, seq) ?: return Result.failure()
        if (chunk.state == ChunkState.CLOUD_PERSISTED.name) return Result.success()

        val recording = recordingDao.get(recordingId) ?: return Result.failure()
        if (recording.state == RecordingState.DELETE_PENDING.name || recording.state == RecordingState.DELETED.name) {
            return Result.success()
        }

        val state = ChunkState.valueOf(chunk.state)
        if (state == ChunkState.FAILED_PERMANENT) {
            return Result.failure() // 永久失败不重试。
        }
        if (state == ChunkState.WRITING) {
            return Result.retry() // 写入未完成：等写入器提交后再试。
        }
        if (state == ChunkState.PERSISTED) {
            chunkDao.updateState(recordingId, seq, ChunkState.UPLOAD_PENDING.name, now())
        }

        val file = File(chunk.filePath)
        if (!file.exists()) {
            failPermanent(chunkDao, recordingId, seq, chunk.attempts, "file_missing")
            return Result.failure()
        }

        val token = graph.settings.deviceToken()
        if (token.isNullOrBlank()) {
            return Result.retry() // 未配对：等用户在设置页配对后再重试。
        }

        // 文件哈希校验（与 Room 行一致才允许上传）。
        val sha256 = hashFile(file)
        if (sha256 != chunk.sha256) {
            failPermanent(chunkDao, recordingId, seq, chunk.attempts, "checksum_mismatch")
            return Result.failure()
        }

        val pcm = file.readBytes()
        val meta = ChunkMeta(
            sampleOffset = chunk.sampleOffset,
            sampleCount = chunk.sampleCount,
            durationMs = chunk.sampleCount * 1000L / recording.sampleRate,
            contentSha256 = chunk.sha256,
            recordedAt = null,
        )

        chunkDao.updateUploadState(
            recordingId, seq, ChunkState.UPLOADING.name,
            chunk.attempts + 1, chunk.lastError, now(),
        )

        val api = graph.api()
        return try {
            // createRecording 幂等（201 新建 / 200 duplicate），保证云端行存在。
            api.createRecording(
                RecordingCreateRequest(
                    recordingId = recordingId,
                    codec = recording.codec,
                    sampleRate = recording.sampleRate,
                    channels = recording.channels,
                    recordedAt = recording.startedAtEpochMs?.let { rfc3339(it) },
                ),
            )
            val commit = api.putChunk(recordingId, seq, meta, pcm)
            if (!commit.durable) throw IOException("server ack not durable: state=${commit.state}")
            chunkDao.updateState(recordingId, seq, ChunkState.CLOUD_PERSISTED.name, now())
            maybeEnqueueFinish(recordingDao, recordingId)
            Result.success()
        } catch (e: ApiError) {
            val attempts = chunk.attempts + 1
            if (e.isPermanent) {
                failPermanent(chunkDao, recordingId, seq, attempts, e.code)
                Result.failure()
            } else {
                chunkDao.updateUploadState(
                    recordingId, seq,
                    ChunkStateMachine.onRetryableFailure(ChunkState.UPLOADING).name,
                    attempts, e.code, now(),
                )
                Result.retry()
            }
        } catch (e: IOException) {
            chunkDao.updateUploadState(
                recordingId, seq,
                ChunkStateMachine.onRetryableFailure(ChunkState.UPLOADING).name,
                chunk.attempts + 1, "io_error", now(),
            )
            Result.retry()
        }
    }

    private suspend fun maybeEnqueueFinish(
        recordingDao: com.recordingmcp.recorder.data.db.RecordingDao,
        recordingId: String,
    ) {
        val recording = recordingDao.get(recordingId) ?: return
        if (recording.state != RecordingState.COMPLETE.name) return
        val chunks = graph.db.chunkDao().listForRecording(recordingId)
        if (chunks.isNotEmpty() && chunks.all { it.state == ChunkState.CLOUD_PERSISTED.name }) {
            UploadScheduler.enqueueFinish(applicationContext, recordingId)
        }
    }

    private suspend fun failPermanent(
        chunkDao: com.recordingmcp.recorder.data.db.ChunkDao,
        recordingId: String,
        seq: Int,
        attempts: Int,
        error: String,
    ) {
        val from = chunkDao.get(recordingId, seq)?.let { ChunkState.valueOf(it.state) } ?: ChunkState.UPLOADING
        chunkDao.updateUploadState(
            recordingId, seq,
            ChunkStateMachine.onPermanentFailure(from).name,
            attempts + 1, error, now(),
        )
    }

    private fun now(): Long = System.currentTimeMillis()

    companion object {
        fun rfc3339(epochMs: Long): String =
            Instant.ofEpochMilli(epochMs).atOffset(ZoneOffset.UTC).format(DateTimeFormatter.ISO_OFFSET_DATE_TIME)

        fun hashFile(file: File): String {
            val digest = MessageDigest.getInstance("SHA-256")
            file.inputStream().use { input ->
                val buf = ByteArray(64 * 1024)
                while (true) {
                    val n = input.read(buf)
                    if (n <= 0) break
                    digest.update(buf, 0, n)
                }
            }
            return digest.digest().toLowerHex()
        }
    }
}


