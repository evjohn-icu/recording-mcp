package com.recordingmcp.recorder.sync

import com.recordingmcp.recorder.audio.AudioCapture
import com.recordingmcp.recorder.data.db.ChunkEntity
import com.recordingmcp.recorder.data.db.ChunkState
import com.recordingmcp.recorder.data.db.RecordingEntity
import com.recordingmcp.recorder.data.db.RecordingState
import com.recordingmcp.recorder.data.remote.ApiError
import com.recordingmcp.recorder.data.remote.FinishRequest
import com.recordingmcp.recorder.data.remote.FinishResponse
import com.recordingmcp.recorder.data.remote.RecordingDetail
import java.io.IOException

/**
 * finish 决策与网络工作流（纯 JVM：数据入参 + 注入远端 [FinishRemote]，无 android 依赖，可单测）。
 *
 * 不变量（ingest-v1 冻结契约，wire 不变）：
 * - finish 载荷必须等于「已提交分块」之和（total_chunks/total_samples/total_duration_ms 均 ≥ 1）；
 * - 显式零块完成（0 块）不可构造 finish —— 本地直接成功，零网络请求；
 * - 本地 manifest 不完整（缺行 / seq 缺口 / 未全部 CLOUD_PERSISTED）→ 等待分块，不 POST 不完整总量；
 * - 本地持久化汇总与重算不一致（旧版本把丢弃尾块计入总量）→ 先 GET 确认：云端 complete 且
 *   等于持久化汇总 → 成功且不 POST（保留云端已保存的 finished 汇总）；否则按重算总量 POST；
 *   真实 conflict_payload 不当成功（永久失败）。
 * 幂等重放由服务端保证：同载荷重放 200 且不重复事件；不一致 → conflict_payload。
 */
interface FinishRemote {
    suspend fun getRecording(recordingId: String): RecordingDetail
    suspend fun finish(recordingId: String, request: FinishRequest): FinishResponse
}

/** finish 汇总（ingest-v1 载荷形状）。 */
data class FinishTotals(val totalChunks: Int, val totalSamples: Long, val totalDurationMs: Long)

/** [FinishWorkflow.run] 的结果：由 FinishWorker 映射到 Result 与本地状态推进。 */
sealed interface FinishOutcome {
    /** 显式零块完成：本地成功，零网络请求。 */
    data object SucceededWithoutNetwork : FinishOutcome

    /** 本地 manifest 不完整：等待分块（Worker retry）。 */
    data object AwaitingChunks : FinishOutcome

    /** GET 确认云端已按本地持久化汇总完成：成功且不 POST。 */
    data object SucceededRemoteComplete : FinishOutcome

    /** finish durable ACK。 */
    data class Completed(val response: FinishResponse) : FinishOutcome

    /** 服务端 sequence_gap：Worker 按 missing_sequences 重新入队后 retry。 */
    data class SequenceGap(val missingSequences: List<Int>) : FinishOutcome

    /** 永久码（conflict_payload 等）：Worker failure。 */
    data class FailedPermanent(val code: String) : FinishOutcome

    /** 网络 / 5xx / 429 / 非 durable ACK：Worker retry。 */
    data object TransientFailure : FinishOutcome
}

object FinishWorkflow {

    /** 显式零块完成：本地 COMPLETE 且 totalChunks=0（含旧版本 (0, 捕获采样, 捕获时长) 形状）。 */
    fun isExplicitZeroComplete(recording: RecordingEntity): Boolean =
        recording.state == RecordingState.COMPLETE.name && recording.totalChunks == 0

    /**
     * 决策 + 网络。远程错误按既有语义传播：sequence_gap 携带缺失 seq，永久码 → 永久失败，
     * 网络/5xx/429/非 durable → 可重试。
     */
    suspend fun run(
        recording: RecordingEntity,
        chunks: List<ChunkEntity>,
        remote: FinishRemote,
        sampleRate: Int = AudioCapture.SAMPLE_RATE,
    ): FinishOutcome {
        if (isExplicitZeroComplete(recording)) return FinishOutcome.SucceededWithoutNetwork

        val chunkSamples = chunks.sumOf { it.sampleCount.toLong() }
        val persisted = FinishTotals(
            totalChunks = recording.totalChunks ?: chunks.size,
            totalSamples = recording.totalSamples ?: chunkSamples,
            totalDurationMs = recording.durationMs ?: (chunkSamples * 1000 / sampleRate),
        )

        // 真实 manifest 完成：seq 0..total-1 连续且全部 CLOUD_PERSISTED，才允许构造 finish。
        val expected = recording.totalChunks ?: chunks.size
        val contiguous = chunks.map { it.seq }.sorted() == (0 until expected).toList()
        val allPersisted = chunks.all { it.state == ChunkState.CLOUD_PERSISTED.name }
        if (chunks.isEmpty() || !contiguous || !allPersisted) return FinishOutcome.AwaitingChunks

        val recomputed = FinishTotals(chunks.size, chunkSamples, chunkSamples * 1000 / sampleRate)

        if (recomputed != persisted) {
            // 旧版本含丢弃尾块的持久化汇总：先 GET 确认云端视图（避免真实冲突被当成功）。
            val detail = try {
                remote.getRecording(recording.recordingId)
            } catch (e: ApiError) {
                return if (e.isPermanent) FinishOutcome.FailedPermanent(e.code) else FinishOutcome.TransientFailure
            } catch (e: IOException) {
                return FinishOutcome.TransientFailure
            }
            val remoteMatchesPersisted = detail.state == "complete" &&
                detail.totalChunks == persisted.totalChunks &&
                detail.totalSamples == persisted.totalSamples &&
                detail.totalDurationMs == persisted.totalDurationMs
            if (remoteMatchesPersisted) return FinishOutcome.SucceededRemoteComplete
        }

        return send(remote, recording.recordingId, recomputed)
    }

    private suspend fun send(remote: FinishRemote, recordingId: String, totals: FinishTotals): FinishOutcome =
        try {
            val response = remote.finish(
                recordingId,
                FinishRequest(
                    totalChunks = totals.totalChunks,
                    totalSamples = totals.totalSamples,
                    totalDurationMs = totals.totalDurationMs,
                ),
            )
            if (response.durable) FinishOutcome.Completed(response) else FinishOutcome.TransientFailure
        } catch (e: ApiError) {
            when {
                e.code == "sequence_gap" -> FinishOutcome.SequenceGap(e.missingSequences())
                e.isPermanent -> FinishOutcome.FailedPermanent(e.code)
                else -> FinishOutcome.TransientFailure
            }
        } catch (e: IOException) {
            FinishOutcome.TransientFailure
        }
}
