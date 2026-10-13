package com.recordingmcp.recorder.ui

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
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Slider
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.recordingmcp.recorder.Graph
import com.recordingmcp.recorder.data.settings.SettingsStore
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/** 设置页：服务地址、配对/撤销凭据、块时长、仅 Wi-Fi、删除已上传音频、隐私说明。 */
@Composable
fun SettingsScreen(graph: Graph) {
    val scope = rememberCoroutineScope()
    val settings = remember { graph.settings }

    val serverUrl by settings.serverUrlFlow.collectAsState(initial = SettingsStore.DEFAULT_SERVER_URL)
    val deviceId by settings.deviceIdFlow.collectAsState(initial = null)
    val chunkSeconds by settings.chunkSecondsFlow.collectAsState(initial = SettingsStore.DEFAULT_CHUNK_SECONDS)
    val wifiOnly by settings.wifiOnlyFlow.collectAsState(initial = false)
    val deleteAfterAck by settings.deleteAfterAckFlow.collectAsState(initial = false)

    var serverInput by remember(serverUrl) { mutableStateOf(serverUrl) }
    var pairingCodeInput by remember { mutableStateOf("") }
    var deviceNameInput by remember { mutableStateOf(android.os.Build.MODEL ?: "Pixel 8") }
    var message by remember { mutableStateOf<String?>(null) }

    Column(
        Modifier
            .fillMaxSize()
            .verticalScroll(rememberScrollState())
            .padding(16.dp),
        verticalArrangement = Arrangement.spacedBy(14.dp),
    ) {
        // ---- 服务地址 ----
        Card(Modifier.fillMaxWidth()) {
            Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("服务地址", style = MaterialTheme.typography.titleSmall)
                OutlinedTextField(
                    value = serverInput,
                    onValueChange = { serverInput = it },
                    label = { Text("ingest 服务 URL") },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )
                Button(onClick = {
                    settings.setServerUrl(serverInput)
                    message = "服务地址已保存"
                }) { Text("保存") }
            }
        }

        // ---- 配对 ----
        Card(Modifier.fillMaxWidth()) {
            Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("设备配对", style = MaterialTheme.typography.titleSmall)
                if (deviceId != null) {
                    Text("已配对：$deviceId", style = MaterialTheme.typography.bodySmall)
                    OutlinedButton(onClick = {
                        settings.clearDeviceCredentials()
                        message = "已撤销本地凭据（云端设备仍需管理面 revoke）"
                    }) { Text("撤销本地凭据") }
                } else {
                    OutlinedTextField(
                        value = pairingCodeInput,
                        onValueChange = { pairingCodeInput = it.uppercase().take(32) },
                        label = { Text("配对码（32 位 base32）") },
                        singleLine = true,
                        modifier = Modifier.fillMaxWidth(),
                    )
                    OutlinedTextField(
                        value = deviceNameInput,
                        onValueChange = { deviceNameInput = it.take(128) },
                        label = { Text("设备名称") },
                        singleLine = true,
                        modifier = Modifier.fillMaxWidth(),
                    )
                    Button(onClick = {
                        scope.launch {
                            message = runCatching {
                                val api = graph.api()
                                val resp = withContext(Dispatchers.IO) {
                                    api.pair(pairingCodeInput.trim(), deviceNameInput.ifBlank { null })
                                }
                                settings.setDeviceCredentials(resp.deviceId, resp.deviceToken)
                                pairingCodeInput = ""
                                "配对成功：${resp.deviceId}"
                            }.getOrElse { "配对失败：${it.message}" }
                        }
                    }) { Text("配对") }
                }
            }
        }

        // ---- 上传偏好 ----
        Card(Modifier.fillMaxWidth()) {
            Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("上传偏好", style = MaterialTheme.typography.titleSmall)
                Text("分块时长：${chunkSeconds}s（15–60s）", style = MaterialTheme.typography.bodyMedium)
                Slider(
                    value = chunkSeconds.toFloat(),
                    onValueChange = { settings.setChunkSeconds(it.toInt()) },
                    valueRange = 15f..60f,
                    steps = 8,
                )
                ToggleRow("仅 Wi-Fi 上传", wifiOnly) { settings.setWifiOnly(it) }
                ToggleRow("云端确认后删除本地音频", deleteAfterAck) { settings.setDeleteAfterAck(it) }
            }
        }

        // ---- 隐私说明 ----
        Card(Modifier.fillMaxWidth()) {
            Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
                Text("存储与隐私", style = MaterialTheme.typography.titleSmall)
                Text(
                    "录音仅保存在应用私有目录并经你配置的服务上传；APK 内不含 ASR 密钥或云端管理凭据，" +
                        "只有你配对得到的设备 token。删除录音会尽力删除本地文件与云端对象，" +
                        "但已派生的记忆数据不会被回溯删除。设备 token 以 SHA-256 存于服务端。",
                    style = MaterialTheme.typography.bodySmall,
                )
            }
        }

        message?.let {
            Text(it, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.primary)
        }
    }
}

@Composable
private fun ToggleRow(label: String, checked: Boolean, onChange: (Boolean) -> Unit) {
    Row(
        Modifier.fillMaxWidth(),
        horizontalArrangement = Arrangement.SpaceBetween,
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(label, style = MaterialTheme.typography.bodyMedium)
        Switch(checked = checked, onCheckedChange = onChange)
    }
}
