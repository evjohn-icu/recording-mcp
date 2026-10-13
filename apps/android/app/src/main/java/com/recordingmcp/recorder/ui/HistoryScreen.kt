package com.recordingmcp.recorder.ui

import android.content.Intent
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.core.content.FileProvider
import com.recordingmcp.recorder.Graph
import com.recordingmcp.recorder.data.db.ChunkState
import com.recordingmcp.recorder.data.db.RecordingState
import com.recordingmcp.recorder.sync.UploadScheduler
import com.recordingmcp.recorder.ui.state.displayStateOf
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import java.io.File
import java.io.FileOutputStream
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale
import java.util.zip.ZipEntry
import java.util.zip.ZipOutputStream

/** 历史页：录音列表 + 每录音同步进度 + 导出（zip / FileProvider ACTION_SEND）+ 删除（确认 → DELETE_PENDING）。 */
@Composable
fun HistoryScreen(graph: Graph) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val recordings by graph.db.recordingDao().observeAll().collectAsState(initial = emptyList())
    var pendingDelete by remember { mutableStateOf<String?>(null) }
    var message by remember { mutableStateOf<String?>(null) }

    if (recordings.isEmpty()) {
        Column(
            Modifier.fillMaxSize().padding(16.dp),
            verticalArrangement = Arrangement.Center,
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            Text("暂无录音", style = MaterialTheme.typography.bodyLarge)
        }
        return
    }

    LazyColumn(
        Modifier.fillMaxSize().padding(12.dp),
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        items(recordings, key = { it.recordingId }) { recording ->
            val chunks = graph.db.chunkDao().observeForRecording(recording.recordingId)
                .collectAsState(initial = emptyList()).value
            val total = chunks.size
            val uploaded = chunks.count { it.state == ChunkState.CLOUD_PERSISTED.name }
            val failedPermanent = chunks.count { it.state == ChunkState.FAILED_PERMANENT.name }
            val display = displayStateOf(
                recordingState = RecordingState.valueOf(recording.state),
                chunksTotal = total,
                chunksCloudPersisted = uploaded,
                chunksFailedPermanent = failedPermanent,
                syncedAt = recording.syncedAt,
                transcriptionState = recording.transcriptionState,
            )

            Card(Modifier.fillMaxWidth()) {
                Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
                    Text(
                        "${DATE_FORMAT.format(Date(recording.createdAt))}  ${recording.recordingId.take(8)}",
                        style = MaterialTheme.typography.titleSmall,
                    )
                    Text(
                        "时长 ${recording.durationMs ?: 0} ms · $uploaded/$total 块云端持久化" +
                            (recording.transcriptionState?.let { " · 转写 $it" } ?: ""),
                        style = MaterialTheme.typography.bodySmall,
                    )
                    Text(
                        displayLabel(display),
                        style = MaterialTheme.typography.labelLarge,
                        color = if (display == com.recordingmcp.recorder.ui.state.DisplayState.PARTIAL_FAILURE) {
                            MaterialTheme.colorScheme.error
                        } else {
                            MaterialTheme.colorScheme.primary
                        },
                    )
                    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        OutlinedButton(onClick = {
                            scope.launch {
                                message = runCatching {
                                    exportRecordingZip(context, graph, recording.recordingId)
                                    "已导出（zip 分享）"
                                }.getOrElse { "导出失败：${it.message}" }
                            }
                        }) { Text("导出") }
                        OutlinedButton(onClick = { pendingDelete = recording.recordingId }) {
                            Text("删除")
                        }
                    }
                }
            }
        }
        message?.let {
            item { Text(it, style = MaterialTheme.typography.bodySmall) }
        }
    }

    pendingDelete?.let { recordingId ->
        AlertDialog(
            onDismissRequest = { pendingDelete = null },
            title = { Text("删除录音？") },
            text = {
                Text(
                    "将标记为 DELETE_PENDING：Reconcile 会尽力删除云端对象并清除本地文件与分块行。" +
                        "已派生的记忆数据不会被回溯删除。",
                )
            },
            confirmButton = {
                Button(onClick = {
                    scope.launch {
                        graph.db.recordingDao().updateState(recordingId, RecordingState.DELETE_PENDING.name)
                        UploadScheduler.enqueueReconcileOnce(context)
                    }
                    pendingDelete = null
                }) { Text("删除") }
            },
            dismissButton = {
                TextButton(onClick = { pendingDelete = null }) { Text("取消") }
            },
        )
    }
}

private val DATE_FORMAT = SimpleDateFormat("yyyy-MM-dd HH:mm", Locale.US)

internal fun displayLabel(state: com.recordingmcp.recorder.ui.state.DisplayState): String = when (state) {
    com.recordingmcp.recorder.ui.state.DisplayState.LOCAL_ONLY -> "仅本地"
    com.recordingmcp.recorder.ui.state.DisplayState.UPLOADING -> "上传中"
    com.recordingmcp.recorder.ui.state.DisplayState.CLOUD_PERSISTED -> "云端已持久化"
    com.recordingmcp.recorder.ui.state.DisplayState.SYNCED -> "已同步"
    com.recordingmcp.recorder.ui.state.DisplayState.TRANSCRIPTION_PENDING -> "转写待处理"
    com.recordingmcp.recorder.ui.state.DisplayState.IN_MEMORY -> "已进入记忆库"
    com.recordingmcp.recorder.ui.state.DisplayState.PARTIAL_FAILURE -> "部分失败"
}

/** 打包录音目录内全部分块为 zip（cache/exports/<recordingId>.zip）并发起分享。 */
internal suspend fun exportRecordingZip(context: android.content.Context, graph: Graph, recordingId: String) =
    withContext(Dispatchers.IO) {
        val dir = File(graph.audioRoot, recordingId)
        require(dir.isDirectory) { "本地音频不存在" }
        val exports = File(context.cacheDir, "exports").apply { mkdirs() }
        val zip = File(exports, "$recordingId.zip")
        ZipOutputStream(FileOutputStream(zip)).use { out ->
            dir.listFiles()?.filter { it.isFile }?.sortedBy { it.name }?.forEach { file ->
                out.putNextEntry(ZipEntry(file.name))
                file.inputStream().use { it.copyTo(out) }
                out.closeEntry()
            }
        }
        val uri = FileProvider.getUriForFile(context, "${context.packageName}.files", zip)
        val send = Intent(Intent.ACTION_SEND).apply {
            type = "application/zip"
            putExtra(Intent.EXTRA_STREAM, uri)
            addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
        }
        context.startActivity(Intent.createChooser(send, "导出录音"))
        zip.absolutePath
    }
