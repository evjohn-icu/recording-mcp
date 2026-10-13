package com.recordingmcp.recorder.data.db

import androidx.room.Dao
import androidx.room.Insert
import androidx.room.OnConflictStrategy
import androidx.room.Query
import kotlinx.coroutines.flow.Flow

@Dao
interface RecordingDao {

    @Insert(onConflict = OnConflictStrategy.REPLACE)
    suspend fun insert(recording: RecordingEntity)

    @Query("SELECT * FROM recordings ORDER BY createdAt DESC")
    fun observeAll(): Flow<List<RecordingEntity>>

    @Query("SELECT * FROM recordings WHERE recordingId = :recordingId")
    suspend fun get(recordingId: String): RecordingEntity?

    @Query("SELECT * FROM recordings WHERE state = 'RECORDING' ORDER BY createdAt DESC LIMIT 1")
    suspend fun getActive(): RecordingEntity?

    @Query("SELECT * FROM recordings WHERE state = 'RECORDING' ORDER BY createdAt DESC LIMIT 1")
    fun observeActive(): Flow<RecordingEntity?>

    @Query("UPDATE recordings SET state = :state WHERE recordingId = :recordingId")
    suspend fun updateState(recordingId: String, state: String)

    @Query(
        "UPDATE recordings SET state = :state, totalChunks = :totalChunks, " +
            "totalSamples = :totalSamples, durationMs = :durationMs WHERE recordingId = :recordingId",
    )
    suspend fun updateFinished(
        recordingId: String,
        state: String,
        totalChunks: Int,
        totalSamples: Long,
        durationMs: Long,
    )

    @Query(
        "UPDATE recordings SET syncedAt = :syncedAt, transcriptionState = :transcriptionState " +
            "WHERE recordingId = :recordingId",
    )
    suspend fun updateSyncInfo(recordingId: String, syncedAt: Long?, transcriptionState: String?)

    @Query("DELETE FROM recordings WHERE recordingId = :recordingId")
    suspend fun delete(recordingId: String)
}

@Dao
interface ChunkDao {

    @Insert(onConflict = OnConflictStrategy.REPLACE)
    suspend fun insert(chunk: ChunkEntity)

    @Query("SELECT * FROM chunks WHERE recordingId = :recordingId AND seq = :seq")
    suspend fun get(recordingId: String, seq: Int): ChunkEntity?

    @Query("SELECT * FROM chunks WHERE recordingId = :recordingId ORDER BY seq")
    suspend fun listForRecording(recordingId: String): List<ChunkEntity>

    @Query("SELECT * FROM chunks WHERE recordingId = :recordingId ORDER BY seq")
    fun observeForRecording(recordingId: String): Flow<List<ChunkEntity>>

    @Query("SELECT * FROM chunks WHERE state = 'WRITING'")
    suspend fun listWriting(): List<ChunkEntity>

    @Query("SELECT * FROM chunks WHERE state = 'UPLOADING'")
    suspend fun listUploading(): List<ChunkEntity>

    @Query("SELECT * FROM chunks WHERE state IN ('UPLOAD_PENDING', 'FAILED_RETRYABLE')")
    suspend fun listRequeueable(): List<ChunkEntity>

    @Query("SELECT * FROM chunks WHERE state = 'FAILED_PERMANENT'")
    suspend fun listPermanentFailures(): List<ChunkEntity>

    @Query(
        "UPDATE chunks SET state = :state, updatedAt = :updatedAt WHERE recordingId = :recordingId AND seq = :seq",
    )
    suspend fun updateState(recordingId: String, seq: Int, state: String, updatedAt: Long)

    @Query(
        "UPDATE chunks SET state = :state, attempts = :attempts, lastError = :lastError, updatedAt = :updatedAt " +
            "WHERE recordingId = :recordingId AND seq = :seq",
    )
    suspend fun updateUploadState(
        recordingId: String,
        seq: Int,
        state: String,
        attempts: Int,
        lastError: String?,
        updatedAt: Long,
    )

    @Query("DELETE FROM chunks WHERE recordingId = :recordingId AND seq = :seq")
    suspend fun delete(recordingId: String, seq: Int)

    @Query("DELETE FROM chunks WHERE recordingId = :recordingId")
    suspend fun deleteForRecording(recordingId: String)
}
