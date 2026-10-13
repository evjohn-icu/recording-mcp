package com.recordingmcp.recorder.ui.state

import com.recordingmcp.recorder.data.db.RecordingState
import org.junit.Assert.assertEquals
import org.junit.Test

/** 七种展示态映射与优先级测试。 */
class DisplayStateTest {

    private fun map(
        recordingState: RecordingState = RecordingState.RECORDING,
        total: Int = 4,
        uploaded: Int = 0,
        failed: Int = 0,
        syncedAt: Long? = null,
        transcriptionState: String? = null,
    ): DisplayState = displayStateOf(recordingState, total, uploaded, failed, syncedAt, transcriptionState)

    @Test
    fun `all local`() {
        assertEquals(DisplayState.LOCAL_ONLY, map())
        assertEquals(DisplayState.LOCAL_ONLY, map(total = 0))
        assertEquals(DisplayState.LOCAL_ONLY, map(recordingState = RecordingState.DELETED))
        assertEquals(DisplayState.LOCAL_ONLY, map(recordingState = RecordingState.DELETE_PENDING, uploaded = 2))
    }

    @Test
    fun `uploading when some but not all chunks acked`() {
        assertEquals(DisplayState.UPLOADING, map(total = 4, uploaded = 1))
        assertEquals(DisplayState.UPLOADING, map(total = 4, uploaded = 3))
    }

    @Test
    fun `cloud persisted only when every chunk acked`() {
        assertEquals(DisplayState.CLOUD_PERSISTED, map(total = 4, uploaded = 4))
        assertEquals(DisplayState.CLOUD_PERSISTED, map(recordingState = RecordingState.COMPLETE, total = 2, uploaded = 2))
    }

    @Test
    fun `synced requires cloud sync ack`() {
        assertEquals(DisplayState.SYNCED, map(total = 4, uploaded = 4, syncedAt = 1_700_000_000_000))
        // transcription_state 缺失但 synced_at 存在 → SYNCED。
        assertEquals(
            DisplayState.SYNCED,
            map(total = 4, uploaded = 4, syncedAt = 1_700_000_000_000, transcriptionState = "none"),
        )
    }

    @Test
    fun `transcription pending from cloud state`() {
        assertEquals(
            DisplayState.TRANSCRIPTION_PENDING,
            map(total = 4, uploaded = 4, syncedAt = 1_700_000_000_000, transcriptionState = "pending"),
        )
    }

    @Test
    fun `in memory when transcribed and ingested`() {
        assertEquals(
            DisplayState.IN_MEMORY,
            map(total = 4, uploaded = 4, syncedAt = 1_700_000_000_000, transcriptionState = "ingested"),
        )
    }

    @Test
    fun `partial failure has highest precedence`() {
        // 即使云端已 ingest，也必须先提示失败。
        assertEquals(
            DisplayState.PARTIAL_FAILURE,
            map(total = 4, uploaded = 4, failed = 1, syncedAt = 1_700_000_000_000, transcriptionState = "ingested"),
        )
        assertEquals(
            DisplayState.PARTIAL_FAILURE,
            map(total = 4, uploaded = 1, failed = 1),
        )
        assertEquals(
            DisplayState.PARTIAL_FAILURE,
            map(recordingState = RecordingState.DELETE_PENDING, total = 4, uploaded = 4, failed = 1),
        )
    }

    @Test
    fun `cloud persisted never shown as in memory or synced`() {
        // 上传完成但没有云端 sync ack → 决不能是 SYNCED / IN_MEMORY。
        assertEquals(DisplayState.CLOUD_PERSISTED, map(total = 4, uploaded = 4, syncedAt = null))
        assertEquals(DisplayState.CLOUD_PERSISTED, map(total = 4, uploaded = 4, transcriptionState = "none"))
        assertEquals(DisplayState.CLOUD_PERSISTED, map(total = 4, uploaded = 4, transcriptionState = "skipped"))
    }

    @Test
    fun `all seven states distinct and coverable`() {
        val seen = setOf(
            map(),                                        // LOCAL_ONLY
            map(uploaded = 1),                            // UPLOADING
            map(uploaded = 4),                            // CLOUD_PERSISTED
            map(uploaded = 4, syncedAt = 1L),             // SYNCED
            map(uploaded = 4, syncedAt = 1L, transcriptionState = "pending"),   // TRANSCRIPTION_PENDING
            map(uploaded = 4, syncedAt = 1L, transcriptionState = "ingested"),  // IN_MEMORY
            map(failed = 1),                              // PARTIAL_FAILURE
        )
        assertEquals(DisplayState.entries.size, seen.size)
        assertEquals(7, DisplayState.entries.size)
    }
}
