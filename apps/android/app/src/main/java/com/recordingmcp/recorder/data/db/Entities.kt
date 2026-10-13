package com.recordingmcp.recorder.data.db

import androidx.room.Entity
import androidx.room.Index
import androidx.room.PrimaryKey

/** 本地录音行。state 取值 = [RecordingState].name；syncedAt/transcriptionState 由 ReconcileWorker 从云端回填。 */
@Entity(tableName = "recordings")
data class RecordingEntity(
    @PrimaryKey val recordingId: String,
    val codec: String,
    val sampleRate: Int,
    val channels: Int,
    val startedAtEpochMs: Long?,
    val state: String,
    val totalChunks: Int?,
    val totalSamples: Long?,
    val durationMs: Long?,
    val createdAt: Long,
    val syncedAt: Long? = null,
    val transcriptionState: String? = null,
)

/** 分块行，主键 (recordingId, seq)。state 取值 = [ChunkState].name。 */
@Entity(
    tableName = "chunks",
    primaryKeys = ["recordingId", "seq"],
    indices = [Index("state")],
)
data class ChunkEntity(
    val recordingId: String,
    val seq: Int,
    val sampleOffset: Long,
    val sampleCount: Int,
    val bytes: Long,
    val sha256: String,
    val filePath: String,
    val state: String,
    val attempts: Int = 0,
    val lastError: String? = null,
    val updatedAt: Long,
)
