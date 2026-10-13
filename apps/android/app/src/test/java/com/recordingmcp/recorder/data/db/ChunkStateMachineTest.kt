package com.recordingmcp.recorder.data.db

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test

/** 分块状态机 JVM 测试：全部状态流转，含崩溃后 WRITING 行清理。 */
class ChunkStateMachineTest {

    @Test
    fun `persisted chunk transitions to upload pending`() {
        assertEquals(ChunkState.UPLOAD_PENDING, ChunkStateMachine.onPersisted())
    }

    @Test
    fun `upload start requires pending or retryable`() {
        assertEquals(ChunkState.UPLOADING, ChunkStateMachine.onUploadStart(ChunkState.UPLOAD_PENDING))
        assertEquals(ChunkState.UPLOADING, ChunkStateMachine.onUploadStart(ChunkState.FAILED_RETRYABLE))
        assertThrows(IllegalArgumentException::class.java) {
            ChunkStateMachine.onUploadStart(ChunkState.CLOUD_PERSISTED)
        }
        assertThrows(IllegalArgumentException::class.java) {
            ChunkStateMachine.onUploadStart(ChunkState.WRITING)
        }
    }

    @Test
    fun `durable ack only from uploading`() {
        assertEquals(ChunkState.CLOUD_PERSISTED, ChunkStateMachine.onDurableAck(ChunkState.UPLOADING))
        assertThrows(IllegalArgumentException::class.java) {
            ChunkStateMachine.onDurableAck(ChunkState.UPLOAD_PENDING)
        }
    }

    @Test
    fun `retryable and permanent failures`() {
        assertEquals(
            ChunkState.FAILED_RETRYABLE,
            ChunkStateMachine.onRetryableFailure(ChunkState.UPLOADING),
        )
        assertEquals(
            ChunkState.FAILED_RETRYABLE,
            ChunkStateMachine.onRetryableFailure(ChunkState.FAILED_RETRYABLE),
        )
        assertEquals(
            ChunkState.FAILED_PERMANENT,
            ChunkStateMachine.onPermanentFailure(ChunkState.UPLOADING),
        )
        assertEquals(
            ChunkState.FAILED_PERMANENT,
            ChunkStateMachine.onPermanentFailure(ChunkState.UPLOAD_PENDING),
        )
        assertThrows(IllegalArgumentException::class.java) {
            ChunkStateMachine.onPermanentFailure(ChunkState.CLOUD_PERSISTED)
        }
    }

    @Test
    fun `requeue policy`() {
        assertTrue(ChunkStateMachine.isRequeueable(ChunkState.UPLOAD_PENDING))
        assertTrue(ChunkStateMachine.isRequeueable(ChunkState.FAILED_RETRYABLE))
        assertFalse(ChunkStateMachine.isRequeueable(ChunkState.CLOUD_PERSISTED))
        assertFalse(ChunkStateMachine.isRequeueable(ChunkState.FAILED_PERMANENT))
        assertFalse(ChunkStateMachine.isRequeueable(ChunkState.WRITING))
    }

    @Test
    fun `crash recovery - WRITING row must be cleaned up regardless of file existence`() {
        assertNull("WRITING 行删除（块未持久化）", ChunkStateMachine.resolveAfterCrash(ChunkState.WRITING, fileExists = true))
        assertNull("WRITING 行删除", ChunkStateMachine.resolveAfterCrash(ChunkState.WRITING, fileExists = false))
    }

    @Test
    fun `crash recovery - UPLOADING resumes or fails permanently`() {
        assertEquals(
            ChunkState.UPLOAD_PENDING,
            ChunkStateMachine.resolveAfterCrash(ChunkState.UPLOADING, fileExists = true),
        )
        assertEquals(
            ChunkState.FAILED_PERMANENT,
            ChunkStateMachine.resolveAfterCrash(ChunkState.UPLOADING, fileExists = false),
        )
        // 其它状态原样保留。
        assertEquals(
            ChunkState.UPLOAD_PENDING,
            ChunkStateMachine.resolveAfterCrash(ChunkState.UPLOAD_PENDING, fileExists = true),
        )
        assertEquals(
            ChunkState.CLOUD_PERSISTED,
            ChunkStateMachine.resolveAfterCrash(ChunkState.CLOUD_PERSISTED, fileExists = false),
        )
    }

    @Test
    fun `full happy-path lifecycle ends terminal`() {
        var state = ChunkState.PERSISTED
        state = ChunkStateMachine.onPersisted()
        assertEquals(ChunkState.UPLOAD_PENDING, state)
        state = ChunkStateMachine.onUploadStart(state)
        state = ChunkStateMachine.onDurableAck(state)
        assertEquals(ChunkState.CLOUD_PERSISTED, state)
        assertFalse(ChunkStateMachine.isRequeueable(state))
    }

    @Test
    fun `retry path recovers to durable ack`() {
        var state = ChunkStateMachine.onPersisted()
        state = ChunkStateMachine.onUploadStart(state)
        state = ChunkStateMachine.onRetryableFailure(state)
        state = ChunkStateMachine.onUploadStart(state)
        state = ChunkStateMachine.onDurableAck(state)
        assertEquals(ChunkState.CLOUD_PERSISTED, state)
    }

    @Test
    fun `recording states cover lifecycle`() {
        // DELETE_PENDING → DELETED 由 Reconcile 推进；RECORDING/COMPLETE 由服务推进。
        assertEquals(
            listOf("RECORDING", "COMPLETE", "DELETE_PENDING", "DELETED"),
            RecordingState.entries.map { it.name },
        )
        assertEquals(7, ChunkState.entries.size)
    }
}
