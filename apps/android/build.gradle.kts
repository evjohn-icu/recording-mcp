// recording-mcp Android 录音端（ingest-v1 客户端）。
// 插件版本统一由 gradle/libs.versions.toml 管理；模块内不写版本号。
plugins {
    alias(libs.plugins.android.application) apply false
    alias(libs.plugins.kotlin.compose) apply false
    alias(libs.plugins.kotlin.serialization) apply false
    alias(libs.plugins.ksp) apply false
    alias(libs.plugins.room) apply false
}
