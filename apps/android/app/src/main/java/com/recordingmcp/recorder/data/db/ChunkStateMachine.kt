package com.recordingmcp.recorder.data.db

/**
 * 本地分块状态机（纯 Kotlin，可 JVM 单测）。
 *
 * 状态流转：
 *   （服务落盘提交）PERSISTED -> UPLOAD_PENDING
 *   UPLOAD_PENDING / FAILED_RETRYABLE --start--> UPLOADING --durable ack--> CLOUD_PERSISTED
 *   UPLOADING --retryable failure--> FAILED_RETRYABLE（Reconcile 重新入队）
 *   UPLOADING --permanent failure--> FAILED_PERMANENT（终态，需人工处理）
 *
 * 崩溃恢复（ReconcileWorker 启动时调用 [resolveAfterCrash]）：
 *   WRITING -> null（行删除；.part 孤儿文件由 Reconcile 清理，块未持久化）
 *   UPLOADING + 文件存在 -> UPLOAD_PENDING（重新入队）
 *   UPLOADING + 文件缺失 -> FAILED_PERMANENT
 */
enum class ChunkState {
    WRITING,
    PERSISTED,
    UPLOAD_PENDING,
    UPLOADING,
    CLOUD_PERSISTED,
    FAILED_RETRYABLE,
    FAILED_PERMANENT,
}

/** 录音本地生命周期。DELETE_PENDING 由 ReconcileWorker 推进为云端删除 + 本地清除。 */
enum class RecordingState {
    RECORDING,
    COMPLETE,
    DELETE_PENDING,
    DELETED,
}

object ChunkStateMachine {

    /** 分块已 fsync 并原子 rename 到最终文件名：行从 PERSISTED 推进到 UPLOAD_PENDING 并立即入队。 */
    fun onPersisted(): ChunkState = ChunkState.UPLOAD_PENDING

    /** 能否启动上传（UploadWorker 入口守卫）。 */
    fun canStartUpload(state: ChunkState): Boolean =
        state == ChunkState.UPLOAD_PENDING || state == ChunkState.FAILED_RETRYABLE || state == ChunkState.UPLOADING

    fun onUploadStart(from: ChunkState): ChunkState {
        require(from == ChunkState.UPLOAD_PENDING || from == ChunkState.FAILED_RETRYABLE) {
            "cannot start upload from $from"
        }
        return ChunkState.UPLOADING
    }

    /** 云端 durable ACK（R2 + manifest + 事件三者落定后的 200）。 */
    fun onDurableAck(from: ChunkState): ChunkState {
        require(from == ChunkState.UPLOADING) { "durable ack requires UPLOADING, got $from" }
        return ChunkState.CLOUD_PERSISTED
    }

    fun onRetryableFailure(from: ChunkState): ChunkState {
        require(
            from == ChunkState.UPLOADING ||
                from == ChunkState.UPLOAD_PENDING ||
                from == ChunkState.FAILED_RETRYABLE,
        ) { "retryable failure from invalid state $from" }
        return ChunkState.FAILED_RETRYABLE
    }

    fun onPermanentFailure(from: ChunkState): ChunkState {
        require(from == ChunkState.UPLOADING || from == ChunkState.UPLOAD_PENDING) {
            "permanent failure from invalid state $from"
        }
        return ChunkState.FAILED_PERMANENT
    }

    /** FAILED_RETRYABLE / UPLOAD_PENDING 由 Reconcile 周期性重新入队。 */
    fun isRequeueable(state: ChunkState): Boolean =
        state == ChunkState.UPLOAD_PENDING || state == ChunkState.FAILED_RETRYABLE

    /**
     * 崩溃/进程死亡后的行恢复判定。返回 null 表示该行必须删除（WRITING 块未持久化）。
     */
    fun resolveAfterCrash(state: ChunkState, fileExists: Boolean): ChunkState? = when (state) {
        ChunkState.WRITING -> null
        ChunkState.UPLOADING -> if (fileExists) ChunkState.UPLOAD_PENDING else ChunkState.FAILED_PERMANENT
        else -> state
    }
}
