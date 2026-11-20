package com.recordingmcp.recorder.audio

import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.io.File
import java.util.Base64
import com.recordingmcp.recorder.ContractVectors

/**
 * StopSummary 的 finish 记账测试（plan 2.1）：
 * finish 必须只使用「已提交分块」的汇总 —— totalChunks = committed 数、
 * totalSamples = committed 各块 sampleCount 之和、durationMs = 该和×1000/16000（整毫秒）。
 *
 * 三种夹具：
 *   A. 30 s 完整块 + 0.5 s 丢弃尾块 —— 捕获总量含被丢弃尾块采样，与提交块求和出现分歧；
 *   B. 正好完整块（60 s = 2×30 s，无尾块）—— 两个口径一致；
 *   C. 合法 1 s 尾块（30 s 完整块 + 1 s 尾块）—— 尾块 ≥1 s 被提交，两个口径一致。
 * 外加零提交块夹具 D（<1 s 会话）：没有可 finish 的分块。
 *
 * 冻结契约背景（contracts/ingest-v1）：服务端以 manifest 各块 sample_count 之和校验
 * total_samples，不匹配即 conflict_payload —— 因此「捕获总量」绝不能作为 finish 的 totalSamples。
 */
class PcmStopSummaryTest {

    @get:Rule
    val tmp = TemporaryFolder()

    private val vectorJson by lazy {
        Json.parseToJsonElement(ContractVectors.read("pcm_1s_440hz.json")).jsonObject
    }

    /** 契约向量 PCM：16000 采样（1 s）440 Hz 正弦，32000 B。 */
    private fun oneSecondPcm(): ByteArray =
        Base64.getDecoder().decode(vectorJson.getValue("pcm_base64").jsonPrimitive.content)

    /** 0.5 s 确定性 PCM：8000 采样（16000 B）。 */
    private fun halfSecondPcm(): ByteArray = ByteArray(2 * 8000)

    // ---- 夹具 A：30 s 完整块 + 0.5 s 丢弃尾块 ----

    @Test
    fun `complete chunk plus discarded half second tail - captured totals diverge from committed`() {
        val dir = tmp.newFolder("recA")
        val writer = PcmChunkWriter(dir, chunkSeconds = 30)
        writer.start()

        val oneSecond = oneSecondPcm()
        repeat(30) { writer.write(oneSecond) } // 30 s 边界：chunk 0 立即提交
        writer.write(halfSecondPcm())          // 0.5 s 尾块
        val summary = writer.stop()

        // 已提交：仅完整 30 s 块；尾块 <1 s 被丢弃。
        assertEquals(1, summary.committedCount)
        assertEquals(0, summary.committed.single().seq)
        assertEquals(480000, summary.committed.single().sampleCount)
        assertEquals(0L, summary.committed.single().sampleOffset)
        assertEquals(960000L, summary.committed.single().bytes)
        assertEquals(listOf(1), summary.discardedTailSeqs)
        assertTrue("committed chunk file kept", File(dir, "chunk-00000.pcm").exists())
        assertFalse("discarded tail part deleted", File(dir, "chunk-00001.pcm.part").exists())

        // 用于 finish 的口径：StopSummary.committedSamples（提交块求和）与整毫秒时长。
        assertEquals(480000L, summary.committedSamples)
        assertEquals("integer ms duration from committed chunks", 30000L, summary.committedSamples * 1000 / 16000)

        // 捕获口径单独断言：仍含被丢弃尾块的 8000 采样（JVM 合同保持不变）。
        assertEquals(488000L, summary.totalSamples)
        // pre-fix 的 stopRecording 正是把 summary.totalSamples 写进录音汇总并发送 finish
        // （488000/30500 ms），服务端 manifest 求和为 480000 → conflict_payload。
        assertTrue(
            "captured totalSamples (with discarded tail) must not feed finish totals",
            summary.totalSamples != summary.committedSamples,
        )
    }

    // ---- 夹具 B：正好完整块（60 s = 2×30 s，无尾块） ----

    @Test
    fun `exact complete chunks - committed totals equal captured and match finish shape`() {
        val dir = tmp.newFolder("recB")
        val writer = PcmChunkWriter(dir, chunkSeconds = 30)
        writer.start()

        val oneSecond = oneSecondPcm()
        repeat(60) { writer.write(oneSecond) }
        val summary = writer.stop()

        assertEquals(2, summary.committedCount)
        assertEquals(0, summary.committed[0].seq)
        assertEquals(480000, summary.committed[0].sampleCount)
        assertEquals(0L, summary.committed[0].sampleOffset)
        assertEquals(1, summary.committed[1].seq)
        assertEquals(480000, summary.committed[1].sampleCount)
        assertEquals("offset chain stays contiguous across chunks", 480000L, summary.committed[1].sampleOffset)

        // 无尾块：提交块求和 == 捕获总量，finish 口径无分歧。
        assertEquals(960000L, summary.committedSamples)
        assertEquals(960000L, summary.totalSamples)
        assertEquals("integer ms duration", 60000L, summary.committedSamples * 1000 / 16000)
        // 60 s 边界后为空尾块（0 采样 < 1 s）→ 记为丢弃 seq 2。
        assertEquals(listOf(2), summary.discardedTailSeqs)
    }

    // ---- 夹具 C：30 s 完整块 + 合法 1 s 尾块 ----

    @Test
    fun `complete chunk plus legal one second tail - tail is committed and totals agree`() {
        val dir = tmp.newFolder("recC")
        val writer = PcmChunkWriter(dir, chunkSeconds = 30)
        writer.start()

        val oneSecond = oneSecondPcm()
        repeat(30) { writer.write(oneSecond) } // chunk 0（边界提交）
        writer.write(oneSecond)                // 尾块恰为 1 s → 合法，提交
        val summary = writer.stop()

        assertEquals(2, summary.committedCount)
        assertEquals(1, summary.committed[1].seq)
        assertEquals(16000, summary.committed[1].sampleCount)
        assertEquals(480000L, summary.committed[1].sampleOffset)
        assertEquals(emptyList<Int>(), summary.discardedTailSeqs)

        // 尾块 ≥1 s 已提交：提交块求和 == 捕获总量，finish 口径一致。
        assertEquals(496000L, summary.committedSamples)
        assertEquals(496000L, summary.totalSamples)
        assertEquals("integer ms duration", 31000L, summary.committedSamples * 1000 / 16000)
    }

    // ---- 夹具 D：零提交块（<1 s 会话） ----

    @Test
    fun `sub second session commits zero chunks - captured samples are not finishable`() {
        val dir = tmp.newFolder("recD")
        val writer = PcmChunkWriter(dir, chunkSeconds = 30)
        writer.start()

        writer.write(halfSecondPcm()) // 0.5 s：尾块 <1 s → 丢弃
        val summary = writer.stop()

        assertEquals("zero valid chunks", 0, summary.committedCount)
        assertTrue(summary.committed.isEmpty())
        assertEquals(listOf(0), summary.discardedTailSeqs)
        assertFalse(File(dir, "chunk-00000.pcm").exists())
        assertFalse(File(dir, "chunk-00000.pcm.part").exists())
        assertTrue("dir empty after discard", dir.listFiles().isNullOrEmpty())

        // 捕获口径仍有 8000 采样，但提交块口径是 committedSamples=0 —— 无可构造的 finish。
        assertEquals(8000L, summary.totalSamples)
        assertEquals(0L, summary.committedSamples)
    }
}
