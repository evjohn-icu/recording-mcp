package com.recordingmcp.recorder.audio

import android.annotation.SuppressLint
import android.media.AudioFormat
import android.media.AudioRecord
import android.media.MediaRecorder

/**
 * 麦克风采集：16 kHz / mono / PCM 16-bit。
 * 每次读取 3200 字节 = 100 ms 音频；调用方把读到的字节喂给 [PcmChunkWriter]。
 */
class AudioCapture(private val sampleRate: Int = SAMPLE_RATE) {

    @Volatile
    private var recorder: AudioRecord? = null

    val isCapturing: Boolean
        get() = recorder?.recordingState == AudioRecord.RECORDSTATE_RECORDING

    @SuppressLint("MissingPermission") // 权限由 RecordingService 在启动前显式检查。
    fun start() {
        check(recorder == null) { "capture already started" }
        val minBuffer = AudioRecord.getMinBufferSize(
            sampleRate,
            AudioFormat.CHANNEL_IN_MONO,
            AudioFormat.ENCODING_PCM_16BIT,
        )
        require(minBuffer > 0) { "AudioRecord min buffer invalid: $minBuffer" }
        val bufferSize = maxOf(minBuffer, READ_BYTES * 4)
        val record = AudioRecord(
            MediaRecorder.AudioSource.MIC,
            sampleRate,
            AudioFormat.CHANNEL_IN_MONO,
            AudioFormat.ENCODING_PCM_16BIT,
            bufferSize,
        )
        check(record.state == AudioRecord.STATE_INITIALIZED) { "AudioRecord init failed" }
        record.startRecording()
        recorder = record
    }

    /** 读取一块 PCM；返回读取字节数，失败/停止返回负值。 */
    fun read(buffer: ByteArray): Int =
        recorder?.read(buffer, 0, buffer.size) ?: -1

    fun stop() {
        recorder?.let { record ->
            try {
                if (record.recordingState == AudioRecord.RECORDSTATE_RECORDING) {
                    record.stop()
                }
            } finally {
                record.release()
            }
        }
        recorder = null
    }

    companion object {
        const val SAMPLE_RATE = 16000
        const val READ_BYTES = 3200 // 100 ms @ 16 kHz s16le mono
    }
}
