package com.recordingmcp.recorder

import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.nio.file.Files
import java.nio.file.Path
import kotlin.io.path.exists

/** 从测试工作目录向上查找仓库内 contracts/ingest-v1/vectors（模块目录的 ../../contracts/ingest-v1/vectors）。 */
object ContractVectors {

    fun vectorsDir(): Path {
        var dir: Path = Path.of(System.getProperty("user.dir")).toAbsolutePath()
        repeat(8) {
            val candidate = dir.resolve("contracts/ingest-v1/vectors")
            if (candidate.resolve("pcm_1s_440hz.json").exists()) return candidate
            dir = dir.parent ?: dir
        }
        error("contracts/ingest-v1/vectors not found from ${System.getProperty("user.dir")}")
    }

    fun read(name: String): String = Files.readString(vectorsDir().resolve(name))
}
