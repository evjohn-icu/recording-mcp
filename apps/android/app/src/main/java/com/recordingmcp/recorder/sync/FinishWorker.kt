package com.recordingmcp.recorder.sync

import android.content.Context
import androidx.work.CoroutineWorker
import androidx.work.WorkerParameters
import com.recordingmcp.recorder.Graph
import com.recordingmcp.recorder.data.db.ChunkState
import com.recordingmcp.recorder.data.db.RecordingState
import com.recordingmcp.recorder.data.remote.FinishRequest
import com.recordingmcp.recorder.data.remote.FinishResponse
import com.recordingmcp.recorder.data.remote.RecordingDetail

/**
 * 结束录音 Worker（唯一名 finish-<recordingId>）。决策与网络在 [FinishWorkflow]（纯 JVM 可测）：
 * - 显式零块完成 → 本地成功，零网络（在凭据/分块/网络检查之前，兼容旧版本无凭据的排队任务）；
 * - 本地 manifest 不完整（缺行/缺口/未全部 CLOUD_PERSISTED）→ retry，不 POST 不完整总量；
 * - 200 durable → 本地记录 COMPLETE；
 * - 409 sequence_gap → 按 details.missing_sequences 把缺失块重新置为 UPLOAD_PENDING 并入队，然后 retry；
 * - 永久码（conflict_payload 等）→ failure；
 * - 其它（网络 / 5xx / 429 / 非 durable ACK）→ retry。
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

        // 显式零块完成：本地直接成功且零网络（ingest-v1 禁止零载荷；兼容旧版本无凭据的排队任务）。
        if (FinishWorkflow.isExplicitZeroComplete(recording)) {
            return Result.success()
        }

        val token = graph.settings.deviceToken()
        if (token.isNullOrBlank()) return Result.retry()

        // IngestApi 适配为可注入远端（wire 契约不变；阻塞调用由 CoroutineWorker 的 IO 调度承担）。
        val api = graph.api()
        val remote = object : FinishRemote {
            override suspend fun getRecording(recordingId: String): RecordingDetail =
                api.getRecording(recordingId)

            override suspend fun finish(recordingId: String, request: FinishRequest): FinishResponse =
                api.finish(recordingId, request)
        }

        val chunks = chunkDao.listForRecording(recordingId)
        return when (val outcome = FinishWorkflow.run(recording, chunks, remote, recording.sampleRate)) {
            FinishOutcome.SucceededWithoutNetwork,
            FinishOutcome.SucceededRemoteComplete,
            is FinishOutcome.Completed -> {
                recordingDao.updateState(recordingId, RecordingState.COMPLETE.name)
                Result.success()
            }
            FinishOutcome.AwaitingChunks,
            FinishOutcome.TransientFailure -> Result.retry()
            is FinishOutcome.SequenceGap -> {
                // 服务端缺口：凡本地有行且文件完好的缺失 seq 都重新入队上传（状态回到 UPLOAD_PENDING）。
                // POST 前置条件已保证本地全量 CLOUD_PERSISTED，缺口只可能指向服务端视图 —— 含
                // CLOUD_PERSISTED 行在内必须重传；本地无行或文件缺失无法补传，跳过留待下轮。
                for (seq in outcome.missingSequences) {
                    val chunk = chunkDao.get(recordingId, seq) ?: continue
                    if (!java.io.File(chunk.filePath).exists()) continue
                    chunkDao.updateState(recordingId, seq, ChunkState.UPLOAD_PENDING.name, System.currentTimeMillis())
                    UploadScheduler.enqueueChunk(applicationContext, recordingId, seq)
                }
                Result.retry()
            }
            is FinishOutcome.FailedPermanent -> Result.failure()
        }
    }
}
