package com.recordingmcp.recorder.sync

import com.recordingmcp.recorder.data.db.ChunkEntity
import com.recordingmcp.recorder.data.db.ChunkState
import com.recordingmcp.recorder.data.db.RecordingEntity
import com.recordingmcp.recorder.data.db.RecordingState
import com.recordingmcp.recorder.data.remote.ApiError
import com.recordingmcp.recorder.data.remote.FinishRequest
import com.recordingmcp.recorder.data.remote.FinishResponse
import com.recordingmcp.recorder.data.remote.RecordingDetail
import kotlinx.coroutines.runBlocking
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * FinishWorkflow JVM 回归（plan 2.2，先于实现的红灯；远端为可注入 spy，无网络）：
 * - 显式零块本地完成（新版本 (0,0,0) 与旧版本 (0, 捕获采样, 捕获时长)）→ 本地成功且零网络调用
 *   （兼容旧版本已排队的 WorkManager 任务）；
 * - 本地 manifest 不完整（缺行 / seq 缺口 / 未全部 CLOUD_PERSISTED）→ AwaitingChunks，
 *   不 POST 不完整总量（守护真实 manifest 完成）；
 * - 完整 manifest → POST 重算总量（与 finish_ok 向量的「总量=提交块之和」一致）；
 *   本地持久化汇总与重算不一致（旧版本含丢弃尾块）→ 先 GET 确认：云端 complete 且等于
 *   持久化汇总 → 成功且不 POST，否则按重算总量 POST；真实 conflict_payload 不得当成功；
 * - 远端错误传播语义保持：sequence_gap → 携带缺失 seq（Worker 重排队后 retry），
 *   永久码 → 永久失败，网络失败 / 429 / 非 durable ACK → 可重试。
 */
class FinishWorkflowTest {

    private fun recording(
        totalChunks: Int? = null,
        totalSamples: Long? = null,
        durationMs: Long? = null,
        state: String = RecordingState.COMPLETE.name,
    ) = RecordingEntity(
        recordingId = "r", codec = "pcm_s16le", sampleRate = 16000, channels = 1,
        startedAtEpochMs = 0L, state = state, totalChunks = totalChunks,
        totalSamples = totalSamples, durationMs = durationMs, createdAt = 0L,
    )

    private fun chunk(seq: Int, sampleCount: Int, state: String = ChunkState.CLOUD_PERSISTED.name) = ChunkEntity(
        recordingId = "r", seq = seq, sampleOffset = seq * 480000L, sampleCount = sampleCount,
        bytes = sampleCount * 2L, sha256 = "a".repeat(64), filePath = "/x/chunk-$seq.pcm",
        state = state, updatedAt = 0L,
    )

    private fun remoteDetail(state: String, totalChunks: Int?, totalSamples: Long?, totalDurationMs: Long?) =
        RecordingDetail(
            recordingId = "r", state = state,
            totalChunks = totalChunks, totalSamples = totalSamples, totalDurationMs = totalDurationMs,
        )

    private fun gapError(vararg missing: Int) = ApiError(
        status = 409,
        code = "sequence_gap",
        message = "missing",
        details = buildJsonObject {
            put("missing_sequences", JsonArray(missing.map { JsonPrimitive(it) }))
        },
    )

    // ---- Reconcile 守卫：显式零块完成识别 ----

    @Test
    fun `reconcile guard detects explicit zero complete rows only`() {
        assertTrue(FinishWorkflow.isExplicitZeroComplete(recording(0, 0, 0)))
        assertTrue("legacy shape: zero chunks with captured-only residue", FinishWorkflow.isExplicitZeroComplete(recording(0, 8000, 500)))
        assertFalse(FinishWorkflow.isExplicitZeroComplete(recording(null, null, null)))
        assertFalse(FinishWorkflow.isExplicitZeroComplete(recording(1, 480000, 30000)))
        assertFalse("zero totals are not zero-complete until locally COMPLETE", FinishWorkflow.isExplicitZeroComplete(recording(0, 8000, 500, RecordingState.RECORDING.name)))
    }

    // ---- 显式零块：本地成功，零网络 ----

    @Test
    fun `explicit zero completion succeeds without any remote call`() = runBlocking {
        val spy = FakeRemote()
        // 新版本 (0,0,0)、旧版本 (0, 捕获采样, 捕获时长)、以及与零块矛盾的多余 chunk 行。
        assertEquals(FinishOutcome.SucceededWithoutNetwork, FinishWorkflow.run(recording(0, 0, 0), emptyList(), spy))
        assertEquals(FinishOutcome.SucceededWithoutNetwork, FinishWorkflow.run(recording(0, 8000, 500), emptyList(), spy))
        assertEquals(FinishOutcome.SucceededWithoutNetwork, FinishWorkflow.run(recording(0, 0, 0), listOf(chunk(0, 480000)), spy))
        assertEquals("zero rows must not touch remote", 0, spy.getRecordingCalls)
        assertEquals(0, spy.finishCalls)
    }

    @Test
    fun `zero totals on a not yet completed row still await chunks`() = runBlocking {
        val spy = FakeRemote()
        assertEquals(FinishOutcome.AwaitingChunks, FinishWorkflow.run(recording(0, 8000, 500, RecordingState.RECORDING.name), emptyList(), spy))
        assertEquals(0, spy.getRecordingCalls)
        assertEquals(0, spy.finishCalls)
    }

    // ---- 本地 manifest 不完整：等待分块，不 POST 不完整总量 ----

    @Test
    fun `missing chunk rows await instead of posting incomplete totals`() = runBlocking {
        val spy = FakeRemote()
        assertEquals(FinishOutcome.AwaitingChunks, FinishWorkflow.run(recording(3, 496000, 31000), listOf(chunk(0, 480000)), spy))
        assertEquals(0, spy.getRecordingCalls)
        assertEquals(0, spy.finishCalls)
    }

    @Test
    fun `sequence gap in local rows awaits`() = runBlocking {
        val spy = FakeRemote()
        assertEquals(FinishOutcome.AwaitingChunks, FinishWorkflow.run(recording(3, 496000, 31000), listOf(chunk(0, 480000), chunk(2, 16000)), spy))
        assertEquals(0, spy.getRecordingCalls)
        assertEquals(0, spy.finishCalls)
    }

    @Test
    fun `chunks not all cloud persisted await instead of posting`() = runBlocking {
        val spy = FakeRemote()
        assertEquals(FinishOutcome.AwaitingChunks, FinishWorkflow.run(recording(1, 488000, 30500), listOf(chunk(0, 480000, ChunkState.UPLOAD_PENDING.name)), spy))
        assertEquals(0, spy.getRecordingCalls)
        assertEquals(0, spy.finishCalls)
    }

    @Test
    fun `no chunk rows without explicit totals await`() = runBlocking {
        val spy = FakeRemote()
        assertEquals(FinishOutcome.AwaitingChunks, FinishWorkflow.run(recording(), emptyList(), spy))
        assertEquals(0, spy.getRecordingCalls)
        assertEquals(0, spy.finishCalls)
    }

    // ---- 完整 manifest：POST 重算总量（wire 不变） ----

    @Test
    fun `consistent totals post once without verification get`() = runBlocking {
        val spy = FakeRemote()
        val outcome = FinishWorkflow.run(recording(1, 480000, 30000), listOf(chunk(0, 480000)), spy)
        assertEquals(FinishOutcome.Completed(spy.finishResponse), outcome)
        assertEquals("consistent totals need no GET", 0, spy.getRecordingCalls)
        assertEquals(1, spy.finishCalls)
        assertEquals(FinishRequest(totalChunks = 1, totalSamples = 480000, totalDurationMs = 30000), spy.finishRequests.single())
    }

    @Test
    fun `legal one second tail totals post the committed chunk sum`() = runBlocking {
        val spy = FakeRemote()
        FinishWorkflow.run(recording(2, 496000, 31000), listOf(chunk(0, 480000), chunk(1, 16000)), spy)
        assertEquals(FinishRequest(totalChunks = 2, totalSamples = 496000, totalDurationMs = 31000), spy.finishRequests.single())
    }

    @Test
    fun `null totals fall back to chunks and post directly when consistent`() = runBlocking {
        val spy = FakeRemote()
        val outcome = FinishWorkflow.run(recording(), listOf(chunk(0, 480000)), spy)
        assertEquals(FinishOutcome.Completed(spy.finishResponse), outcome)
        assertEquals(0, spy.getRecordingCalls)
        assertEquals(FinishRequest(totalChunks = 1, totalSamples = 480000, totalDurationMs = 30000), spy.finishRequests.single())
    }

    // ---- 持久化汇总与重算不一致（旧版本含丢弃尾块）：先 GET 确认 ----

    @Test
    fun `legacy tail including totals succeed when remote already complete with persisted totals`() = runBlocking {
        val spy = FakeRemote(detail = remoteDetail("complete", 1, 488000, 30500))
        val outcome = FinishWorkflow.run(recording(1, 488000, 30500), listOf(chunk(0, 480000)), spy)
        assertEquals(FinishOutcome.SucceededRemoteComplete, outcome)
        assertEquals(1, spy.getRecordingCalls)
        assertEquals("remote already complete with persisted totals: no finish POST", 0, spy.finishCalls)
    }

    @Test
    fun `legacy tail including totals post recomputed when remote is not complete`() = runBlocking {
        val spy = FakeRemote(detail = remoteDetail("recording", 1, 488000, 30500))
        FinishWorkflow.run(recording(1, 488000, 30500), listOf(chunk(0, 480000)), spy)
        assertEquals(1, spy.getRecordingCalls)
        assertEquals(FinishRequest(totalChunks = 1, totalSamples = 480000, totalDurationMs = 30000), spy.finishRequests.single())
    }

    @Test
    fun `legacy totals against different remote totals hit real conflict payload as permanent`() = runBlocking {
        // 云端已按 480000/30000 完成（不同于本地持久化的 488000/30500）→ GET 确认未命中 → POST 重算 → conflict_payload。
        val spy = FakeRemote(
            detail = remoteDetail("complete", 1, 480000, 30000),
            finishError = ApiError(status = 409, code = "conflict_payload", message = "recording already complete with different totals"),
        )
        val outcome = FinishWorkflow.run(recording(1, 488000, 30500), listOf(chunk(0, 480000)), spy)
        assertEquals(FinishOutcome.FailedPermanent("conflict_payload"), outcome)
        assertEquals(1, spy.finishCalls)
    }

    @Test
    fun `verification get failure is transient and posts nothing`() = runBlocking {
        val spy = FakeRemote(detailError = java.io.IOException("offline"))
        val outcome = FinishWorkflow.run(recording(1, 488000, 30500), listOf(chunk(0, 480000)), spy)
        assertEquals(FinishOutcome.TransientFailure, outcome)
        assertEquals(0, spy.finishCalls)
    }

    // ---- 远端错误传播（既有语义保持） ----

    @Test
    fun `sequence gap from server carries missing sequences for requeue`() = runBlocking {
        val spy = FakeRemote(finishError = gapError(1, 2))
        val outcome = FinishWorkflow.run(recording(1, 480000, 30000), listOf(chunk(0, 480000)), spy)
        assertEquals(FinishOutcome.SequenceGap(listOf(1, 2)), outcome)
    }

    @Test
    fun `conflict payload on consistent totals is permanent`() = runBlocking {
        val spy = FakeRemote(finishError = ApiError(status = 409, code = "conflict_payload", message = "sum mismatch"))
        val outcome = FinishWorkflow.run(recording(1, 480000, 30000), listOf(chunk(0, 480000)), spy)
        assertEquals(FinishOutcome.FailedPermanent("conflict_payload"), outcome)
    }

    @Test
    fun `retryable codes network failures and non durable acks are transient`() = runBlocking {
        val rateLimited = FakeRemote(finishError = ApiError(status = 429, code = "rate_limited", message = "slow down"))
        assertEquals(FinishOutcome.TransientFailure, FinishWorkflow.run(recording(1, 480000, 30000), listOf(chunk(0, 480000)), rateLimited))

        val offline = FakeRemote(finishError = java.io.IOException("offline"))
        assertEquals(FinishOutcome.TransientFailure, FinishWorkflow.run(recording(1, 480000, 30000), listOf(chunk(0, 480000)), offline))

        val notDurable = FakeRemote(finishResponse = FinishResponse(state = "complete", totalChunks = 1, totalSamples = 480000, durable = false))
        assertEquals(FinishOutcome.TransientFailure, FinishWorkflow.run(recording(1, 480000, 30000), listOf(chunk(0, 480000)), notDurable))
    }
}

/** 注入远端 spy：记录调用次数与 finish 请求载荷；按需返回响应或抛异常（无真实网络）。 */
private class FakeRemote(
    private val detail: RecordingDetail? = null,
    private val detailError: Exception? = null,
    val finishResponse: FinishResponse = FinishResponse(state = "complete", totalChunks = 1, totalSamples = 48000, durable = true),
    private val finishError: Exception? = null,
) : FinishRemote {
    var getRecordingCalls = 0
        private set
    var finishCalls = 0
        private set
    val finishRequests = mutableListOf<FinishRequest>()

    override suspend fun getRecording(recordingId: String): RecordingDetail {
        getRecordingCalls++
        detailError?.let { throw it }
        return detail ?: error("unexpected getRecording (no detail configured)")
    }

    override suspend fun finish(recordingId: String, request: FinishRequest): FinishResponse {
        finishCalls++
        finishRequests += request
        finishError?.let { throw it }
        return finishResponse
    }
}
