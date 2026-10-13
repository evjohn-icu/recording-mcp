package com.recordingmcp.recorder.audio

import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertArrayEquals
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
 * 分块写入器 JVM 测试：滚动边界（30/60 s）、尾块 <1 s 丢弃、
 * 与 contracts/ingest-v1/vectors/pcm_1s_440hz.json 的 sha256 与字节级一致。
 */
class PcmChunkWriterTest {

    @get:Rule
    val tmp = TemporaryFolder()

    private val vectorJson by lazy {
        Json.parseToJsonElement(ContractVectors.read("pcm_1s_440hz.json")).jsonObject
    }

    /** 契约向量 PCM：16000 采样（1 s）440 Hz 正弦。 */
    private fun vectorPcm(): ByteArray =
        Base64.getDecoder().decode(vectorJson.getValue("pcm_base64").jsonPrimitive.content)

    private fun vectorSha(): String =
        vectorJson.getValue("content_sha256").jsonPrimitive.content

    /** 生成 n 秒的确定性 PCM（与向量同公式，仅用于填充时长）。 */
    private fun sineSeconds(seconds: Int): ByteArray {
        val sampleRate = 16000
        val samples = seconds * sampleRate
        val out = ByteArray(samples * 2)
        for (i in 0 until samples) {
            val v = Math.round(1000.0 * Math.sin(2.0 * Math.PI * 440.0 * i / sampleRate)).toInt().toShort()
            out[i * 2] = (v.toInt() and 0xFF).toByte()
            out[i * 2 + 1] = ((v.toInt() shr 8) and 0xFF).toByte()
        }
        return out
    }

    @Test
    fun `vector pcm round trip - sha256 and bytes match contract vector`() {
        val dir = tmp.newFolder("rec1")
        val writer = PcmChunkWriter(dir, chunkSeconds = 30)
        val committed = mutableListOf<CommittedChunk>()
        writer.onChunkCommitted = { committed.add(it) }
        writer.start()

        val pcm = vectorPcm()
        writer.write(pcm)
        writer.stop()

        assertEquals("exactly one committed chunk", 1, committed.size)
        val chunk = committed.single()
        assertEquals(vectorSha(), chunk.sha256)
        assertArrayEquals("chunk file bytes equal vector pcm", pcm, chunk.file.readBytes())
        assertEquals(16000, chunk.sampleCount)
        assertEquals(0L, chunk.sampleOffset)
        assertEquals(32000L, chunk.bytes)
        // 首尾 8 字节（契约向量字段 first_8/last_8_bytes_hex）。
        val fileBytes = chunk.file.readBytes()
        assertEquals("0000ac005301ef01", fileBytes.take(8).toByteArray().toLowerHex())
        assertEquals("83fd11feadfe54ff", fileBytes.takeLast(8).toByteArray().toLowerHex())
        assertFalse(File(dir, "chunk-00000.pcm.part").exists())
    }

    @Test
    fun `rolls at 30s boundary - chunk0 complete after 30 blocks`() {
        val dir = tmp.newFolder("rec2")
        val writer = PcmChunkWriter(dir, chunkSeconds = 30)
        val committed = mutableListOf<CommittedChunk>()
        writer.onChunkCommitted = { committed.add(it) }
        writer.start()

        val oneSecond = vectorPcm()
        repeat(30) { writer.write(oneSecond) }

        assertEquals("chunk 0 complete exactly at 30 s", 1, committed.size)
        assertEquals(0, committed[0].seq)
        assertEquals(30 * 16000, committed[0].sampleCount)
        assertEquals(0L, committed[0].sampleOffset)
        assertEquals(960000L, committed[0].bytes)
        assertTrue(File(dir, "chunk-00000.pcm").exists())
        assertFalse(File(dir, "chunk-00000.pcm.part").exists())
        assertTrue(File(dir, "chunk-00001.pcm.part").exists())

        repeat(30) { writer.write(oneSecond) }
        assertEquals(2, committed.size)
        assertEquals(1, committed[1].seq)
        assertEquals(480000L, committed[1].sampleOffset)

        val summary = writer.stop()
        assertEquals(2, summary.committedCount)
        assertEquals(960000L, summary.totalSamples)
        // 第 60 s 后无写入：空尾块（0 采样 < 1 s）被丢弃。
        assertEquals(listOf(2), summary.discardedTailSeqs)
    }

    @Test
    fun `rolls at 60s boundary - chunk0 complete after 60 blocks`() {
        val dir = tmp.newFolder("rec3")
        val writer = PcmChunkWriter(dir, chunkSeconds = 60)
        val committed = mutableListOf<CommittedChunk>()
        writer.onChunkCommitted = { committed.add(it) }
        writer.start()

        val oneSecond = vectorPcm()
        repeat(59) { writer.write(oneSecond) }
        assertEquals("no roll before 60 s", 0, committed.size)

        writer.write(oneSecond)
        assertEquals("chunk 0 complete exactly at 60 s", 1, committed.size)
        assertEquals(960000, committed[0].sampleCount)
        assertEquals(1920000L, committed[0].bytes)

        // 再写 1 s：进入 chunk 1，停止时尾块恰为 1 s → 保留。
        writer.write(oneSecond)
        val summary = writer.stop()
        assertEquals(2, summary.committedCount)
        assertEquals(16000, summary.committed[1].sampleCount)
    }

    @Test
    fun `tail shorter than 1s is discarded and part file deleted`() {
        val dir = tmp.newFolder("rec4")
        val writer = PcmChunkWriter(dir, chunkSeconds = 30)
        val committed = mutableListOf<CommittedChunk>()
        val discarded = mutableListOf<Int>()
        writer.onChunkCommitted = { committed.add(it) }
        writer.onTailDiscarded = { discarded.add(it) }
        writer.start()

        writer.write(ByteArray(2 * 8000)) // 0.5 s
        val summary = writer.stop()

        assertTrue("no committed chunks", committed.isEmpty())
        assertEquals(listOf(0), discarded)
        assertEquals(0, summary.committedCount)
        assertEquals(8000L, summary.totalSamples)
        assertFalse(File(dir, "chunk-00000.pcm").exists())
        assertFalse(File(dir, "chunk-00000.pcm.part").exists())
        assertTrue("dir empty after discard", dir.listFiles().isNullOrEmpty())
    }

    @Test
    fun `tail at least 1s is kept as final chunk`() {
        val dir = tmp.newFolder("rec5")
        val writer = PcmChunkWriter(dir, chunkSeconds = 30)
        val committed = mutableListOf<CommittedChunk>()
        writer.onChunkCommitted = { committed.add(it) }
        writer.start()

        val oneSecond = vectorPcm()
        repeat(5) { writer.write(oneSecond) }
        val summary = writer.stop()

        assertEquals(1, summary.committedCount)
        assertEquals(0, summary.committed[0].seq)
        assertEquals(80000, summary.committed[0].sampleCount)
    }

    @Test
    fun `write across chunk boundary splits samples correctly`() {
        val dir = tmp.newFolder("rec6")
        // 3 秒块，用 2 秒 + 2 秒两次写入：chunk0 = 3s（48000 样本），chunk1 = 1s 尾块保留。
        val writer = PcmChunkWriter(dir, chunkSeconds = 3)
        val committed = mutableListOf<CommittedChunk>()
        writer.onChunkCommitted = { committed.add(it) }
        writer.start()

        writer.write(sineSeconds(2))
        writer.write(sineSeconds(2))
        val summary = writer.stop()

        assertEquals(2, summary.committedCount)
        assertEquals(48000, committed[0].sampleCount)
        assertEquals(16000, committed[1].sampleCount)
        assertEquals(48000L, committed[1].sampleOffset)
        assertEquals(64000L, summary.totalSamples)
    }
}
