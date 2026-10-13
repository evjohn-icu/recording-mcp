package com.recordingmcp.recorder.data.remote

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.jsonObject
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import java.io.IOException
import java.util.Base64
import java.util.concurrent.TimeUnit

// ---------------------------------------------------------------------------
// 协议字面量（与 contracts/ingest-v1/openapi.yaml 严格镜像）
// ---------------------------------------------------------------------------

/** ingest-v1 协议版本，所有 /v1 请求必须携带 X-Ingest-Version 头。 */
const val PROTOCOL_VERSION = "ingest-v1"

const val HEADER_INGEST_VERSION = "X-Ingest-Version"
const val HEADER_CHUNK_META = "X-Chunk-Meta"
const val HEADER_AUTHORIZATION = "Authorization"

@Serializable
data class PairRequest(
    @SerialName("protocol_version") val protocolVersion: String = PROTOCOL_VERSION,
    @SerialName("pairing_code") val pairingCode: String,
    @SerialName("device_name") val deviceName: String? = null,
)

@Serializable
data class PairResponse(
    @SerialName("device_id") val deviceId: String,
    val owner: String,
    @SerialName("device_token") val deviceToken: String,
    @SerialName("server_protocol_version") val serverProtocolVersion: String,
)

@Serializable
data class RecordingCreateRequest(
    @SerialName("protocol_version") val protocolVersion: String = PROTOCOL_VERSION,
    @SerialName("recording_id") val recordingId: String,
    val codec: String = "pcm_s16le",
    @SerialName("sample_rate") val sampleRate: Int = 16000,
    val channels: Int = 1,
    @SerialName("recorded_at") val recordedAt: String? = null,
    val timezone: String? = null,
)

@Serializable
data class RecordingCreateResponse(
    @SerialName("recording_id") val recordingId: String,
    val state: String,
    @SerialName("created_at") val createdAt: String,
    val duplicate: Boolean = false,
)

/**
 * X-Chunk-Meta 头的载荷。字段顺序与 chunk_meta_ok.json 的 canonical JSON 一致
 * （sample_offset, sample_count, duration_ms, content_sha256, recorded_at）。
 */
@Serializable
data class ChunkMeta(
    @SerialName("sample_offset") val sampleOffset: Long,
    @SerialName("sample_count") val sampleCount: Int,
    @SerialName("duration_ms") val durationMs: Long,
    @SerialName("content_sha256") val contentSha256: String,
    @SerialName("recorded_at") val recordedAt: String? = null,
)

/** base64url(compact JSON) —— 与契约向量 chunk_meta_ok.json 的 x_chunk_meta_base64url 逐字节一致。 */
fun ChunkMeta.toBase64UrlHeader(): String {
    val json = META_JSON.encodeToString(ChunkMeta.serializer(), this)
    return Base64.getUrlEncoder().withoutPadding().encodeToString(json.toByteArray(Charsets.UTF_8))
}

/** ChunkMeta 专用：显式输出 "recorded_at":null（向量 canonical JSON 含该键）。 */
private val META_JSON: Json = Json { encodeDefaults = true }

@Serializable
data class ChunkCommitResponse(
    val seq: Int,
    val state: String,
    @SerialName("r2_key") val r2Key: String,
    val checksum: String,
    val durable: Boolean,
    val duplicate: Boolean = false,
)

@Serializable
data class FinishRequest(
    @SerialName("protocol_version") val protocolVersion: String = PROTOCOL_VERSION,
    @SerialName("total_chunks") val totalChunks: Int,
    @SerialName("total_samples") val totalSamples: Long,
    @SerialName("total_duration_ms") val totalDurationMs: Long,
)

@Serializable
data class FinishResponse(
    val state: String,
    @SerialName("total_chunks") val totalChunks: Int,
    @SerialName("total_samples") val totalSamples: Long,
    val durable: Boolean,
)

@Serializable
data class SyncInfo(
    @SerialName("synced_at") val syncedAt: String? = null,
    @SerialName("transcription_state") val transcriptionState: String? = null,
)

@Serializable
data class RecordingDetail(
    @SerialName("recording_id") val recordingId: String,
    val state: String,
    val codec: String? = null,
    @SerialName("sample_rate") val sampleRate: Int? = null,
    val channels: Int? = null,
    @SerialName("recorded_at") val recordedAt: String? = null,
    @SerialName("created_at") val createdAt: String? = null,
    @SerialName("finished_at") val finishedAt: String? = null,
    @SerialName("total_chunks") val totalChunks: Int? = null,
    @SerialName("total_samples") val totalSamples: Long? = null,
    @SerialName("total_duration_ms") val totalDurationMs: Long? = null,
    val sync: SyncInfo? = null,
)

@Serializable
data class ErrorDetail(
    val code: String,
    val message: String,
    val details: JsonObject? = null,
)

@Serializable
data class ErrorEnvelope(
    val error: ErrorDetail,
)

/** 非错误体的失败（网络层抛 IOException；HTTP 非 2xx 抛本类）。 */
class ApiError(
    val status: Int,
    val code: String,
    override val message: String,
    val details: JsonObject? = null,
) : RuntimeException("ingest $status $code: $message") {

    /** 永久失败：重试也不会成功（内容/校验冲突类）。 */
    val isPermanent: Boolean get() = code in PERMANENT_CODES

    /** sequence_gap 的 details.missing_sequences。 */
    fun missingSequences(): List<Int> =
        (details?.get("missing_sequences") as? JsonArray)
            ?.mapNotNull { (it as? JsonPrimitive)?.content?.toIntOrNull() }
            ?: emptyList()

    companion object {
        val PERMANENT_CODES: Set<String> =
            setOf("validation_failed", "conflict_checksum", "conflict_payload")
    }
}

// ---------------------------------------------------------------------------
// 客户端
// ---------------------------------------------------------------------------

/**
 * ingest-v1 客户端（纯 JVM：OkHttp + kotlinx.serialization，无 android.* 依赖，可直接单测）。
 * 所有方法阻塞式执行（Worker 在 IO 调度器上调用；测试用 runBlocking）。
 * 网络失败抛 IOException；HTTP 非 2xx 抛 [ApiError]。
 */
class IngestApi(
    baseUrl: String,
    private val tokenProvider: () -> String? = { null },
    private val client: OkHttpClient = defaultClient(),
) {

    private val base = baseUrl.trimEnd('/')

    /** 常规 API JSON：忽略未知字段、省略 null（pair 的 device_name 为可省字符串）。 */
    private val json: Json = Json {
        ignoreUnknownKeys = true
        explicitNulls = false
        encodeDefaults = true
    }

    /** 一次性交换配对码（无认证；10/min/IP 限速由服务端执行）。 */
    fun pair(pairingCode: String, deviceName: String? = null): PairResponse {
        val body = json.encodeToString(PairRequest.serializer(), PairRequest(pairingCode = pairingCode, deviceName = deviceName))
        val request = Request.Builder()
            .url("$base/v1/devices/pair")
            .header(HEADER_INGEST_VERSION, PROTOCOL_VERSION)
            .post(body.toRequestBody(JSON_MEDIA_TYPE))
            .build()
        return execute(request) { json.decodeFromString(PairResponse.serializer(), it) }
    }

    /** 创建录音（幂等：同 id 同字段重放 → 200 duplicate）。 */
    fun createRecording(rec: RecordingCreateRequest): RecordingCreateResponse {
        val body = json.encodeToString(RecordingCreateRequest.serializer(), rec)
        val request = authenticatedBuilder("$base/v1/recordings")
            .post(body.toRequestBody(JSON_MEDIA_TYPE))
            .build()
        return execute(request) { json.decodeFromString(RecordingCreateResponse.serializer(), it) }
    }

    /** 上传原始 PCM 分块（durable ACK：200 仅在 R2 + manifest + 事件三者落定后返回）。 */
    fun putChunk(recordingId: String, seq: Int, meta: ChunkMeta, pcm: ByteArray): ChunkCommitResponse {
        val request = authenticatedBuilder("$base/v1/recordings/$recordingId/chunks/$seq")
            .header(HEADER_CHUNK_META, meta.toBase64UrlHeader())
            .put(pcm.toRequestBody(OCTET_STREAM_MEDIA_TYPE))
            .build()
        return execute(request) { json.decodeFromString(ChunkCommitResponse.serializer(), it) }
    }

    /** 结束录音；缺口返回 409 sequence_gap + details.missing_sequences。 */
    fun finish(recordingId: String, req: FinishRequest): FinishResponse {
        val body = json.encodeToString(FinishRequest.serializer(), req)
        val request = authenticatedBuilder("$base/v1/recordings/$recordingId/finish")
            .post(body.toRequestBody(JSON_MEDIA_TYPE))
            .build()
        return execute(request) { json.decodeFromString(FinishResponse.serializer(), it) }
    }

    fun getRecording(recordingId: String): RecordingDetail {
        val request = authenticatedBuilder("$base/v1/recordings/$recordingId")
            .get()
            .build()
        return execute(request) { json.decodeFromString(RecordingDetail.serializer(), it) }
    }

    /** 删除录音（幂等；已删除仍返回 200）。 */
    fun deleteRecording(recordingId: String): RecordingDetail {
        val request = authenticatedBuilder("$base/v1/recordings/$recordingId")
            .delete()
            .build()
        return execute(request) { json.decodeFromString(RecordingDetail.serializer(), it) }
    }

    // ---- 内部 ----

    private fun authenticatedBuilder(url: String): Request.Builder {
        val builder = Request.Builder()
            .url(url)
            .header(HEADER_INGEST_VERSION, PROTOCOL_VERSION)
        val token = tokenProvider()
        if (!token.isNullOrBlank()) {
            builder.header(HEADER_AUTHORIZATION, "Bearer $token")
        }
        return builder
    }

    private fun <T> execute(request: Request, parse: (String) -> T): T {
        val response = client.newCall(request).execute()
        response.use { resp ->
            val bodyStr = resp.body?.string().orEmpty()
            if (!resp.isSuccessful) {
                throw parseError(resp.code, bodyStr)
            }
            return parse(bodyStr)
        }
    }

    private fun parseError(status: Int, body: String): ApiError {
        val envelope = runCatching { json.decodeFromString(ErrorEnvelope.serializer(), body) }.getOrNull()
        val detail = envelope?.error
        return if (detail != null) {
            ApiError(status, detail.code, detail.message, detail.details)
        } else {
            ApiError(status, "internal", body.take(200))
        }
    }

    companion object {
        private val JSON_MEDIA_TYPE = "application/json; charset=utf-8".toMediaType()
        private val OCTET_STREAM_MEDIA_TYPE = "application/octet-stream".toMediaType()

        fun defaultClient(): OkHttpClient = OkHttpClient.Builder()
            .connectTimeout(15, TimeUnit.SECONDS)
            .readTimeout(60, TimeUnit.SECONDS)
            .writeTimeout(60, TimeUnit.SECONDS)
            .build()
    }
}

/** 网络层异常统一别名（Worker 判定可重试时捕获 IOException/[ApiError]）。 */
class NetworkUnavailableException(cause: IOException) : IOException(cause.message, cause) {
    constructor() : this(IOException("network unavailable"))
}

/** 从 RFC3339 字符串解析 epoch 毫秒（minSdk 26 自带 java.time）。 */
fun parseRfc3339ToEpochMs(value: String?): Long? =
    value?.let {
        runCatching { java.time.OffsetDateTime.parse(it).toInstant().toEpochMilli() }.getOrNull()
    }
