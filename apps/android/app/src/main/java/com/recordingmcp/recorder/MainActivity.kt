package com.recordingmcp.recorder

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.NavigationBar
import androidx.compose.material3.NavigationBarItem
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import com.recordingmcp.recorder.ui.HistoryScreen
import com.recordingmcp.recorder.ui.RecorderScreen
import com.recordingmcp.recorder.ui.SettingsScreen

class MainActivity : ComponentActivity() {

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val graph = Graph.get(applicationContext)
        setContent {
            MaterialTheme {
                AppScaffold(graph)
            }
        }
    }
}

private val TAB_TITLES = listOf("录音", "历史", "设置")

@Composable
private fun AppScaffold(graph: Graph) {
    var tab by remember { mutableIntStateOf(0) }
    Scaffold(
        bottomBar = {
            NavigationBar {
                TAB_TITLES.forEachIndexed { index, title ->
                    NavigationBarItem(
                        selected = tab == index,
                        onClick = { tab = index },
                        icon = { Text(title.take(1)) },
                        label = { Text(title) },
                    )
                }
            }
        },
    ) { padding ->
        Box(Modifier.padding(padding)) {
            when (tab) {
                0 -> RecorderScreen(graph)
                1 -> HistoryScreen(graph)
                else -> SettingsScreen(graph)
            }
        }
    }
}
