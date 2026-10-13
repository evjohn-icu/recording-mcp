package com.recordingmcp.recorder.ui.state

import com.recordingmcp.recorder.data.db.RecordingState

/** 展示态七分类（面向用户的录音同步状态机）。 */
enum class DisplayState {
    LOCAL_ONLY,
    UPLOADING,
    CLOUD_PERSISTED,
    SYNCED,
    TRANSCRIPTION_PENDING,
    IN_MEMORY,
    PARTIAL_FAILURE,
}

/**
 * 展示态映射。
 * 优先级：PARTIAL_FAILURE 最高（有永久失败一律先提示）；
 * 云端 ack（syncedAt）与转写状态来自 context-sync 回写；
 * **CLOUD_PERSISTED 绝不显示为已进入记忆库（SYNCED / IN_MEMORY）** —— 只有云端
 * sync.synced_at / transcription_state 才能触发后两者。
 *
 * @param recordingState 本地录音生命周期
 * @param chunksTotal 已落盘分块总数（0 = 还没有完整分块）
 * @param chunksCloudPersisted 已获 durable ACK 的分块数
 * @param chunksFailedPermanent FAILED_PERMANENT 分块数
 * @param syncedAt 云端 sync.synced_at（epoch ms 或 null）
 * @param transcriptionState 云端 transcription_state
 */
fun displayStateOf(
    recordingState: RecordingState,
    chunksTotal: Int,
    chunksCloudPersisted: Int,
    chunksFailedPermanent: Int,
    syncedAt: Long?,
    transcriptionState: String?,
): DisplayState {
    if (chunksFailedPermanent > 0) return DisplayState.PARTIAL_FAILURE
    if (recordingState == RecordingState.DELETED || recordingState == RecordingState.DELETE_PENDING) {
        return DisplayState.LOCAL_ONLY
    }
    when (transcriptionState) {
        "ingested" -> return DisplayState.IN_MEMORY
        "pending" -> return DisplayState.TRANSCRIPTION_PENDING
    }
    if (syncedAt != null) return DisplayState.SYNCED
    if (chunksTotal > 0 && chunksCloudPersisted == chunksTotal) return DisplayState.CLOUD_PERSISTED
    if (chunksCloudPersisted > 0) return DisplayState.UPLOADING
    return DisplayState.LOCAL_ONLY
}
