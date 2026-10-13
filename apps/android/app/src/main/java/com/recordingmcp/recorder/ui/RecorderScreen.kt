package com.recordingmcp.recorder.ui

import android.Manifest
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableLongStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import com.recordingmcp.recorder.Graph
import com.recordingmcp.recorder.data.db.ChunkEntity
import com.recordingmcp.recorder.data.db.ChunkState
import com.recordingmcp.recorder.data.db.RecordingState
import com.recordingmcp.recorder.service.RecordingService
import kotlinx.coroutines.delay
import java.util.Locale

/** 录音页：开始/停止、已录时长、采集状态、本地已保存大小、上传进度、分块状态。 */
@Composable
fun RecorderScreen(graph: Graph) {
    val context = LocalContext.current
    val recordingDao = remember { graph.db.recordingDao() }
    val chunkDao = remember { graph.db.chunkDao() }

    val active by recordingDao.observeActive().collectAsState(initial = null)
    var chunks by remember { mutableStateOf(emptyList<ChunkEntity>()) }
    LaunchedEffect(active?.recordingId) {
        val id = active?.recordingId
        if (id == null) {
            chunks = emptyList()
        } else {
            chunkDao.observeForRecording(id).collect { chunks = it }
        }
    }

    var now by remember { mutableLongStateOf(System.currentTimeMillis()) }
    LaunchedEffect(Unit) {
        while (true) {
            now = System.currentTimeMillis()
            delay(500)
        }
    }

    val permissionsLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestMultiplePermissions(),
    ) { grants ->
        if (grants[Manifest.permission.RECORD_AUDIO] == true) {
            RecordingService.start(context)
        }
    }

    Column(
        modifier = Modifier
            .fillMaxSize()
            .verticalScroll(rememberScrollState())
            .padding(16.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        val recording = active
        val elapsedText = if (recording?.startedAtEpochMs != null) {
            val seconds = ((now - recording.startedAtEpochMs) / 1000).coerceAtLeast(0)
            String.format(Locale.US, "%02d:%02d", seconds / 60, seconds % 60)
        } else {
            "00:00"
        }

        Text(elapsedText, style = MaterialTheme.typography.displayLarge)

        val isRecording = recording != null
        Button(onClick = {
            if (isRecording) {
                RecordingService.stop(context)
            } else {
                permissionsLauncher.launch(
                    arrayOf(Manifest.permission.RECORD_AUDIO, Manifest.permission.POST_NOTIFICATIONS),
                )
            }
        }) {
            Text(if (isRecording) "停止" else "开始录音")
        }

        Text(
            text = if (isRecording) "采集状态：麦克风采集中（16 kHz / mono / s16le）" else "采集状态：空闲",
            style = MaterialTheme.typography.bodyMedium,
        )

        Card(Modifier.fillMaxWidth()) {
            Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
                Text("本地已保存：${formatBytes(chunks.sumOf { it.bytes })}", style = MaterialTheme.typography.bodyMedium)
                val total = chunks.size
                val uploaded = chunks.count { it.state == ChunkState.CLOUD_PERSISTED.name }
                Text(
                    "云端上传：$uploaded / $total 块已 durable ACK",
                    style = MaterialTheme.typography.bodyMedium,
                )
                if (total > 0) {
                    LinearProgressIndicator(
                        progress = { uploaded.toFloat() / total },
                        modifier = Modifier.fillMaxWidth(),
                    )
                }
            }
        }

        if (chunks.isNotEmpty()) {
            Card(Modifier.fillMaxWidth()) {
                Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    Text("分块状态", style = MaterialTheme.typography.titleSmall)
                    chunks.takeLast(8).reversed().forEach { chunk ->
                        Row(
                            Modifier.fillMaxWidth(),
                            horizontalArrangement = Arrangement.SpaceBetween,
                        ) {
                            Text(
                                "#%05d  %s".format(Locale.US, chunk.seq, formatBytes(chunk.bytes)),
                                style = MaterialTheme.typography.bodySmall,
                            )
                            Text(
                                chunkLabel(chunk.state),
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.primary,
                            )
                        }
                    }
                }
            }
        }

        if (recording?.state == RecordingState.COMPLETE.name) {
            Text("录音已完成，等待云端 finish / 转写。", style = MaterialTheme.typography.bodySmall)
        }
    }
}

internal fun chunkLabel(state: String): String = when (state) {
    ChunkState.WRITING.name -> "写入中"
    ChunkState.PERSISTED.name -> "已落盘"
    ChunkState.UPLOAD_PENDING.name -> "待上传"
    ChunkState.UPLOADING.name -> "上传中"
    ChunkState.CLOUD_PERSISTED.name -> "云端已持久化"
    ChunkState.FAILED_RETRYABLE.name -> "可重试失败"
    ChunkState.FAILED_PERMANENT.name -> "永久失败"
    else -> state
}

internal fun formatBytes(bytes: Long): String = when {
    bytes >= 1L shl 20 -> String.format(Locale.US, "%.1f MB", bytes / 1048576.0)
    bytes >= 1L shl 10 -> String.format(Locale.US, "%.1f KB", bytes / 1024.0)
    else -> "$bytes B"
}
