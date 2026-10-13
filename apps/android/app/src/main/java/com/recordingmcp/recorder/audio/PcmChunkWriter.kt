package com.recordingmcp.recorder.audio

import java.io.File
import java.io.FileOutputStream
import java.security.MessageDigest

/** 一个已完成（fsync + 原子 rename）的分块描述，供服务层写入 Room 并入队上传。 */
data class CommittedChunk(
    val seq: Int,
    val file: File,
    val sha256: String,
    val bytes: Long,
    val sampleCount: Int,
    val sampleOffset: Long,
)

/** [PcmChunkWriter.stop] 的会话汇总。 */
data class StopSummary(
    val committed: List<CommittedChunk>,
    val discardedTailSeqs: List<Int>,
    val totalSamples: Long,
) {
    val committedCount: Int get() = committed.size
}

/**
 * PCM 分块落盘器（纯 JVM：java.io + MessageDigest，无 android 依赖，可单测）。
 *
 * - 输出目录 `<dir>/chunk-<seq:05d>.pcm`，写入期间为 `chunk-<seq:05d>.pcm.part`；
 * - 边写边算 SHA-256；块完成时 `FileDescriptor.sync()` 后原子 `renameTo` 去掉 `.part`；
 * - 达到 chunkSeconds 对应字节数即滚动到下一 seq；
 * - [stop] 时尾块 < 1 s（采样数 < sampleRate）→ 丢弃并删除 `.part`（ingest 契约要求块时长 ≥ 1000 ms）。
 */
class PcmChunkWriter(
    private val dir: File,
    private val chunkSeconds: Int = 30,
    private val sampleRate: Int = AudioCapture.SAMPLE_RATE,
) : AutoCloseable {

    /** 每个分块完成时回调（滚动时同步触发；停止时的尾块包含在 [stop] 返回值里，也会触发本回调）。 */
    @Volatile
    var onChunkCommitted: ((CommittedChunk) -> Unit)? = null

    @Volatile
    var onTailDiscarded: ((seq: Int) -> Unit)? = null

    private var seq: Int = 0
    private var out: FileOutputStream? = null
    private var digest: MessageDigest = MessageDigest.getInstance("SHA-256")
    private var bytesInChunk: Long = 0
    private var chunkStartSampleOffset: Long = 0
    private var totalSamples: Long = 0
    private var started: Boolean = false

    private val allCommitted = mutableListOf<CommittedChunk>()
    private val discardedSeqs = mutableListOf<Int>()
    private val maxChunkBytes: Long = chunkSeconds.toLong() * sampleRate * 2

    val committedChunks: List<CommittedChunk> get() = allCommitted.toList()
    val totalSamplesWritten: Long get() = totalSamples

    @Synchronized
    fun start() {
        check(!started) { "writer already started" }
        check(dir.isDirectory || dir.mkdirs()) { "cannot create chunk dir: $dir" }
        started = true
        openChunk(0)
    }

    /**
     * 写入原始 PCM 字节（s16le；[offset]+[length] 范围内长度必须为偶数）。
     * 块写满即立刻提交并滚动（即使本次 write 恰好在边界结束）。
     */
    @Synchronized
    fun write(pcm: ByteArray, offset: Int, length: Int) {
        check(started) { "writer not started" }
        require(length >= 0 && offset >= 0 && offset + length <= pcm.size) { "bad range" }
        require(length % 2 == 0) { "PCM bytes must be even-length, got $length" }
        var pos = offset
        val end = offset + length
        while (pos < end) {
            val writable = minOf(end - pos, (maxChunkBytes - bytesInChunk).toInt())
            val out = requireNotNull(out)
            out.write(pcm, pos, writable)
            digest.update(pcm, pos, writable)
            bytesInChunk += writable
            totalSamples += writable / 2
            pos += writable
            if (bytesInChunk >= maxChunkBytes && pos < end) {
                completeChunk()
            }
        }
        if (bytesInChunk >= maxChunkBytes) {
            // 本次 write 恰好把块填满：立刻提交（滚动到下一 seq），保证 durable ACK 及时。
            completeChunk()
        }
    }

    /** 便捷入口：写入整个数组。 */
    @Synchronized
    fun write(pcm: ByteArray) {
        write(pcm, 0, pcm.size)
    }

    /** 手动滚动到下一分块（例如暂停时保证块边界）。 */
    @Synchronized
    fun roll() {
        check(started) { "writer not started" }
        if (bytesInChunk > 0) {
            completeChunk()
        }
    }

    /**
     * 结束会话：尾块 < 1 s 时丢弃（删除 .part），否则作为最后一个分块提交。
     * 返回整个会话的汇总（含此前滚动提交的所有分块）。
     */
    @Synchronized
    fun stop(): StopSummary {
        if (!started) {
            return StopSummary(allCommitted.toList(), discardedSeqs.toList(), totalSamples)
        }
        started = false
        val samplesInTail = bytesInChunk / 2
        if (out != null) {
            if (samplesInTail < sampleRate) {
                // 尾块不足 1 s：契约不允许 <1000 ms 的块，丢弃并删除 .part。
                requireNotNull(out).close()
                out = null
                chunkPartFile(seq).delete()
                discardedSeqs.add(seq)
                onTailDiscarded?.invoke(seq)
            } else {
                completeChunk()
            }
        }
        return StopSummary(allCommitted.toList(), discardedSeqs.toList(), totalSamples)
    }

    override fun close() {
        stop()
    }

    // ---- 文件名工具（Reconcile 孤儿清理复用） ----

    fun chunkPartFile(n: Int): File = File(dir, "chunk-%05d.pcm.part".format(n))

    fun chunkFinalFile(n: Int): File = File(dir, "chunk-%05d.pcm".format(n))

    // ---- 内部 ----

    private fun openChunk(n: Int) {
        seq = n
        digest = MessageDigest.getInstance("SHA-256")
        bytesInChunk = 0
        chunkStartSampleOffset = totalSamples
        out = FileOutputStream(chunkPartFile(n))
    }

    private fun completeChunk() {
        val fout = requireNotNull(out)
        fout.fd.sync()
        fout.close()
        out = null
        val part = chunkPartFile(seq)
        val final = chunkFinalFile(seq)
        if (!part.renameTo(final)) {
            // 跨文件系统等极端场景的兜底：copy + delete（同目录 rename 正常总是成功）。
            part.copyTo(final, overwrite = true)
            part.delete()
        }
        val committed = CommittedChunk(
            seq = seq,
            file = final,
            sha256 = digest.digest().toLowerHex(),
            bytes = bytesInChunk,
            sampleCount = (bytesInChunk / 2).toInt(),
            sampleOffset = chunkStartSampleOffset,
        )
        allCommitted.add(committed)
        onChunkCommitted?.invoke(committed)
        openChunk(seq + 1)
    }
}

/** 小写 hex（JVM/ART 通用，避免依赖 kotlin.text.HexFormat 的平台差异）。 */
fun ByteArray.toLowerHex(): String {
    val digits = "0123456789abcdef"
    val sb = StringBuilder(size * 2)
    for (b in this) {
        val v = b.toInt() and 0xFF
        sb.append(digits[v ushr 4]).append(digits[v and 0x0F])
    }
    return sb.toString()
}
