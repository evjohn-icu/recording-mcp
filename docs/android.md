# Android 录音端（apps/android）

Kotlin + Compose + Room + WorkManager 实现的采集端：16 kHz / mono / PCM S16LE 分块录音（默认 30 s，可配 15–60 s），每块落盘持久化后经 ingest-v1 协议上传 Cloudflare，`durable ACK`（HTTP 200 且 `durable:true`）之前绝不删除本地音频。

## 构建前提

- JDK 17+（本机 OpenJDK 26 + Gradle 9.8.1 + AGP 9.4.0 实测可用，无需降级）。
- Android SDK（本仓库开发机安装方式）：

```sh
# commandline tools → $HOME/Android/Sdk/cmdline-tools/latest
# developer.android.com/studio 当前构建号为准（本轮使用 11076708）
yes | sdkmanager --licenses
sdkmanager "platform-tools" "platforms;android-36" "platforms;android-37.0" "build-tools;36.0.0"
# apps/android/local.properties 写入 sdk.dir（已 gitignore）
```

- Gradle wrapper 已随仓库提交（`gradle-9.8.1`），无需本地安装 Gradle。

## 构建与测试

```sh
cd apps/android
ANDROID_HOME=$HOME/Android/Sdk ./gradlew :app:testDebugUnitTest   # 34 个 JVM 单测
ANDROID_HOME=$HOME/Android/Sdk ./gradlew :app:assembleDebug       # 产物 app/build/outputs/apk/debug/app-debug.apk
```

本轮实测：34/34 通过；APK 12,896,644 字节，sha256 `d4e702811e0e32e69dfa3262729ba4edc3c8a078e487f90e71a18d56f4f49ff9`。

## 版本与已知偏差（相对计划）

| 项 | 计划 | 实际 | 原因 |
| --- | --- | --- | --- |
| Kotlin 插件 | `org.jetbrains.kotlin.android` 2.3.0 | 移除（AGP 9 内置 Kotlin） | AGP 9.0+ 再应用该插件会硬报错；compose/serialization 插件照常叠加 |
| KSP | 2.3.0 | 2.3.4 | AGP 9 内置 Kotlin 要求 KSP ≥ 2.3.4 |
| compileSdk | 36 | 37（targetSdk 仍 36，minSdk 26） | Compose BOM 2026.09.00 → ui 1.12.1 要求 compileSdk ≥ 37 |
| Room | 2.8.5 | 2.8.5 | 无需回退 |

## 组件地图

```
app/src/main/java/com/recordingmcp/recorder/
├── RecorderApp.kt            # Application：通知渠道 recording
├── audio/AudioCapture.kt     # AudioRecord MIC 16k mono PCM16，3200 B/读（100 ms）
├── audio/PcmChunkWriter.kt   # chunk-%05d.pcm.part → 边写边算 SHA-256 → fsync → 原子 rename；
│                             #   满 chunkSeconds 滚动；尾块 <1 s 丢弃
├── service/RecordingService.kt  # 前台服务（foregroundServiceType="microphone"），
│                             #   通知显示时长 + 停止 action；START_NOT_STICKY；仅前台启动
├── data/db/                  # Room：recordings / chunks；ChunkState、RecordingState 枚举
├── data/remote/IngestApi.kt  # OkHttp + kotlinx.serialization；X-Ingest-Version + X-Chunk-Meta
├── data/settings/SettingsStore.kt  # serverUrl/deviceId/deviceToken/chunkSeconds/wifiOnly/deleteAfterAck
├── sync/UploadScheduler.kt   # 唯一 work 名 upload-<rid>-<seq>；EXPONENTIAL 30s；KEEP
├── sync/UploadWorker.kt      # 200 → CLOUD_PERSISTED；validation_failed/conflict_checksum/
│                             #   conflict_payload → FAILED_PERMANENT；网络/5xx → retry
├── sync/FinishWorker.kt      # finish；409 sequence_gap → 按 missing_sequences 补传后重试
├── sync/ReconcileWorker.kt   # 周期 15 min：孤儿 .part 清理、断点重排、DELETE_PENDING、
│                             #   轮询 getRecording 回写 syncedAt/transcriptionState
└── ui/                       # 录音 / 历史 / 设置 三页；DisplayState 七态映射
```

## 状态机

- chunk：`WRITING → PERSISTED → UPLOAD_PENDING → UPLOADING → CLOUD_PERSISTED`（或 `FAILED_RETRYABLE` / `FAILED_PERMANENT`）。
- recording：`RECORDING → COMPLETE`（导出/删除路径 `DELETE_PENDING → DELETED`）。
- 展示层 DisplayState：`LOCAL_ONLY → UPLOADING → CLOUD_PERSISTED → SYNCED/TRANSCRIPTION_PENDING/IN_MEMORY`，`PARTIAL_FAILURE` 优先；**CLOUD_PERSISTED 绝不显示为已进入记忆库**（有单测锁定该语义）。

## 配对与凭据

1. 管理员在服务端 `POST /v1/admin/pairing-codes` 签发 32 位 base32 配对码（TTL 默认 15 min，一次性）。
2. App 设置页输入服务地址 + 配对码 → `POST /v1/devices/pair` 换取 `dev_*` 设备 ID 与 token。
3. token 保存在 app 私有 SharedPreferences（自托管单用户场景可接受；升级路径 EncryptedSharedPreferences）。APK 内不含 ASR key、管理 token 或 R2 凭据。

## 权限与隐私

仅声明 `RECORD_AUDIO`、`POST_NOTIFICATIONS`、`INTERNET`、`FOREGROUND_SERVICE`、`FOREGROUND_SERVICE_MICROPHONE`。录音文件存于 app 私有目录，导出走 FileProvider/SAF；删除 = 本地清除 + 云端 `DELETE /v1/recordings/{id}`（离线置 `DELETE_PENDING` 由 Reconcile 重试）。

## 已知限制（如实声明）

- **真机场景未验证**（本轮 Scenario G = NOT_RUN）：无可用设备，`adb devices` 为空；已验证的只有 JVM 单测与 debug APK 构建。
- 不做开机自启 / 隐藏录音：Android 15+ 平台限制后台启动麦克风前台服务，服务仅由用户前台点击启动。
- 上传依赖 WorkManager 网络约束（`CONNECTED` / `wifiOnly` 时 `UNMETERED`），锁屏/断网由已持久化分块 + 恢复协调兜底。
- Room schema 已导出（`app/schemas/.../1.json`），后续迁移必须保持兼容。

## Scenario G 手工步骤（真机，未执行）

1. `adb install -r app/build/outputs/apk/debug/app-debug.apk`；授予 `RECORD_AUDIO`、通知权限。
2. 设置页填 ingest 服务地址 + 配对码，配对成功。
3. 录音 ≥ 2 min → `adb shell dumpsys activity services RecordingService` 观察前台服务与通知。
4. `adb shell cmd jobscheduler run -f com.recordingmcp.recorder 999`（或直接锁屏等待）触发上传；服务端 `GET /v1/recordings/{id}` 确认 chunk 与 finish。
5. 断网（飞行模式）录音 → 恢复网络 → 观察 Reconcile/UploadWorker 补传；服务端核对 `total_chunks` 无缺口。
6. 杀进程（`adb shell am force-stop`）→ 重开 App → 录音列表状态完整（Room 已持久化分块不丢失）。
