package com.recordingmcp.recorder.sync

import com.recordingmcp.recorder.ContractVectors
import com.recordingmcp.recorder.audio.PcmChunkWriter
import com.recordingmcp.recorder.data.remote.FinishRequest
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.util.Base64

/**
 * finish 契约约束测试（plan 2.2 的零提交块前提，JVM 可测部分）：
 * - finish_ok 向量证明服务端期望的汇总 = manifest 各块 sample_count 之和、duration = 采样×1000/16000；
 * - 因此零提交块会话（<1 s 尾块按契约丢弃）只能产出 (0, 0, 0) 的载荷，
 *   而冻结 schema（contracts/ingest-v1/openapi.yaml FinishRequest）要求三字段 minimum ≥ 1 ——
 *   零有效块不可能构造出符合 ingest-v1 的 finish，Worker 必须本地成功而非发送/重试
 *   （该不发网络请求的行为断言由 plan 2.2 的 FinishWorkflowTest 用注入远端覆盖）。
 */
class FinishZeroContractTest {

    @get:Rule
    val tmp = TemporaryFolder()

    @Test
    fun `frozen finish_ok vector totals equal the sum of committed chunks`() {
        val finishOk = Json.parseToJsonElement(ContractVectors.read("finish_ok.json")).jsonObject
        val body = finishOk.getValue("json_body").jsonObject
        val pcm = Base64.getDecoder().decode(
            Json.parseToJsonElement(ContractVectors.read("pcm_1s_440hz.json")).jsonObject
                .getValue("pcm_base64").jsonPrimitive.content,
        )
        val oneSecondSamples = pcm.size / 2
        assertEquals(16000, oneSecondSamples)

        // setup: uploaded seq 0,1,2 of the 1 s sine vector —— finish 汇总必须是提交块之和。
        assertEquals(3, body.getValue("total_chunks").jsonPrimitive.content.toInt())
        assertEquals(3 * oneSecondSamples.toLong(), body.getValue("total_samples").jsonPrimitive.content.toLong())
        assertEquals(3 * 1000L, body.getValue("total_duration_ms").jsonPrimitive.content.toLong())
    }

    @Test
    fun `zero committed chunks can only produce a zero payload that ingest-v1 cannot accept`() {
        // <1 s 会话：尾块按契约（块时长 ≥1000 ms）丢弃，零提交分块。
        val dir = tmp.newFolder("subsecond")
        val writer = PcmChunkWriter(dir, chunkSeconds = 30)
        writer.start()
        writer.write(ByteArray(2 * 8000)) // 0.5 s
        val summary = writer.stop()
        assertEquals(0, summary.committedCount)

        // 从提交块构造 finish（Worker 修复后唯一合法的来源口径）——结果恰为冻结 schema
        // （FinishRequest 三字段 minimum ≥ 1）禁止的零值形状。
        val request = FinishRequest(
            totalChunks = summary.committedCount,
            totalSamples = summary.committedSamples,
            totalDurationMs = summary.committedSamples * 1000 / 16000,
        )
        assertEquals(0, request.totalChunks)
        assertEquals(0L, request.totalSamples)
        assertEquals(0L, request.totalDurationMs)
    }
}
