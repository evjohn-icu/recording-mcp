package com.recordingmcp.recorder.data.remote

import kotlinx.coroutines.runBlocking
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import java.util.Base64
import com.recordingmcp.recorder.ContractVectors

/**
 * IngestApi JVM 测试（MockWebServer，无网络）：
 * - 所有请求携带 X-Ingest-Version: ingest-v1；
 * - X-Chunk-Meta 与契约向量 chunk_meta_ok.json 的 x_chunk_meta_base64url 逐字节一致；
 * - 错误码 → ApiError 映射与 permanent/retryable 判定。
 */
class IngestApiTest {

    private lateinit var server: MockWebServer

    @Before
    fun setUp() {
        server = MockWebServer()
        server.start()
    }

    @After
    fun tearDown() {
        server.shutdown()
    }

    private fun api(token: String? = "test-device-token") =
        IngestApi(server.url("/").toString(), { token })

    private fun enqueueJson(body: String, code: Int = 200) {
        server.enqueue(
            MockResponse().setResponseCode(code).setHeader("Content-Type", "application/json").setBody(body),
        )
    }

    // ---- 协议头 ----

    @Test
    fun `pair sends protocol version body and parses response`() {
        enqueueJson(
            """{"device_id":"dev_018f1a2b-3c4d-5e6f-7a8b-9c0d1e2f3a4b","owner":"default",""" +
                """"device_token":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","server_protocol_version":"ingest-v1"}""",
        )
        val response = runBlocking {
            api(token = null).pair("ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", "Pixel 8")
        }
        assertEquals("dev_018f1a2b-3c4d-5e6f-7a8b-9c0d1e2f3a4b", response.deviceId)
        assertEquals("default", response.owner)
        assertEquals("ingest-v1", response.serverProtocolVersion)

        val recorded = server.takeRequest()
        assertEquals("ingest-v1", recorded.getHeader("X-Ingest-Version"))
        assertNull("pair 无认证头", recorded.getHeader("Authorization"))
        val body = recorded.body.readUtf8()
        assertTrue(body.contains("\"protocol_version\":\"ingest-v1\""))
        assertTrue(body.contains("\"pairing_code\":\"ABCDEFGHIJKLMNOPQRSTUVWXYZ234567\""))
        assertTrue(body.contains("\"device_name\":\"Pixel 8\""))
    }

    @Test
    fun `authenticated requests carry ingest version and bearer token`() = runBlocking {
        enqueueJson("""{"recording_id":"r1","state":"recording","created_at":"2026-10-14T00:00:00Z","duplicate":false}""")
        enqueueJson("""{"state":"complete","total_chunks":2,"total_samples":32000,"durable":true}""")
        enqueueJson(
            """{"recording_id":"r1","state":"complete","codec":"pcm_s16le","sample_rate":16000,"channels":1,""" +
                """"created_at":"2026-10-14T00:00:00Z","chunks":[],"sync":{"synced_at":null,"transcription_state":"none"}}""",
        )
        enqueueJson("""{"recording_id":"r1","state":"deleted","created_at":"2026-10-14T00:00:00Z"}""")

        api().createRecording(RecordingCreateRequest(recordingId = "r1"))
        api().finish("r1", FinishRequest(totalChunks = 2, totalSamples = 32000, totalDurationMs = 2000))
        val detail = api().getRecording("r1")
        api().deleteRecording("r1")

        val requests = listOf(
            server.takeRequest(),
            server.takeRequest(),
            server.takeRequest(),
            server.takeRequest(),
        )
        for (request in requests) {
            assertEquals("ingest-v1", request.getHeader("X-Ingest-Version"))
            assertEquals("Bearer test-device-token", request.getHeader("Authorization"))
        }
        assertEquals("POST", requests[0].method)
        assertEquals("/v1/recordings", requests[0].path)
        assertEquals("/v1/recordings/r1/finish", requests[1].path)
        assertEquals("GET", requests[2].method)
        assertEquals("DELETE", requests[3].method)
        assertEquals("none", detail.sync?.transcriptionState)
    }

    // ---- X-Chunk-Meta 向量一致 ----

    @Test
    fun `chunk meta header matches contract vector byte for byte`() {
        val vector = Json.parseToJsonElement(ContractVectors.read("chunk_meta_ok.json")).jsonObject
        val expectedHeader = vector.getValue("x_chunk_meta_base64url").jsonPrimitive.content
        val expectedJson = vector.getValue("chunk_meta_json").jsonPrimitive.content

        val meta = ChunkMeta(
            sampleOffset = 0,
            sampleCount = 16000,
            durationMs = 1000,
            contentSha256 = "7dc3155cafa10a44695e5aa5cef3fbe68dbdcedc5d00f99a9c0aa5d1dd71a5c3",
            recordedAt = null,
        )
        // base64url(JSON) == 向量字面量；解码回 JSON == canonical chunk_meta_json。
        assertEquals(expectedHeader, meta.toBase64UrlHeader())
        val decoded = Base64.getUrlDecoder().decode(expectedHeader).toString(Charsets.UTF_8)
        assertEquals(expectedJson, decoded)

        // per-seq 向量（seq 1 = offset 16000，finish_ok offset 链）同样可复现。
        val perSeq1 = vector.getValue("per_seq").jsonObject.getValue("1").jsonPrimitive.content
        assertEquals(
            perSeq1,
            meta.copy(sampleOffset = 16000).toBase64UrlHeader(),
        )
    }

    @Test
    fun `putChunk sends octet stream with vector meta header`() = runBlocking {
        val vector = Json.parseToJsonElement(ContractVectors.read("chunk_meta_ok.json")).jsonObject
        val sha = vector.getValue("chunk_meta_json").jsonPrimitive.content
            .let { Json.parseToJsonElement(it).jsonObject.getValue("content_sha256").jsonPrimitive.content }
        enqueueJson(
            """{"seq":0,"state":"committed","r2_key":"owners/default/recordings/r1/chunks/00000000.pcm",""" +
                """"checksum":"$sha","durable":true}""",
        )

        val pcm = ByteArray(32000)
        val meta = ChunkMeta(0, 16000, 1000, sha, null)
        val commit = api().putChunk("r1", 0, meta, pcm)

        assertEquals(0, commit.seq)
        assertEquals("committed", commit.state)
        assertTrue(commit.durable)
        assertFalse(commit.duplicate)

        val recorded = server.takeRequest()
        assertEquals("ingest-v1", recorded.getHeader("X-Ingest-Version"))
        assertEquals(vector.getValue("x_chunk_meta_base64url").jsonPrimitive.content, recorded.getHeader("X-Chunk-Meta"))
        assertEquals("application/octet-stream", recorded.headers["Content-Type"])
        assertEquals("PUT", recorded.method)
        assertEquals("/v1/recordings/r1/chunks/0", recorded.path)
        assertEquals(32000L, recorded.bodySize)
    }

    // ---- 错误映射 ----

    @Test
    fun `error envelope maps to ApiError with status and code`() = runBlocking {
        enqueueJson(
            """{"error":{"code":"conflict_checksum","message":"sha mismatch","details":{}}}""",
            code = 409,
        )
        val error = assertThrows(ApiError::class.java) {
            runBlocking {
                api().putChunk("r1", 0, ChunkMeta(0, 16000, 1000, "a".repeat(64), null), ByteArray(32000))
            }
        }
        assertEquals(409, error.status)
        assertEquals("conflict_checksum", error.code)
        assertTrue(error.isPermanent)
    }

    @Test
    fun `unparseable error body maps to internal`() = runBlocking {
        server.enqueue(MockResponse().setResponseCode(500).setBody("boom"))
        val error = assertThrows(ApiError::class.java) {
            runBlocking { api().getRecording("r1") }
        }
        assertEquals(500, error.status)
        assertEquals("internal", error.code)
        assertFalse(error.isPermanent)
    }

    @Test
    fun `sequence gap details parse missing sequences`() = runBlocking {
        enqueueJson(
            """{"error":{"code":"sequence_gap","message":"missing",""" +
                """"details":{"missing_sequences":[2,5]}}}""",
            code = 409,
        )
        val error = assertThrows(ApiError::class.java) {
            runBlocking {
                api().finish("r1", FinishRequest(totalChunks = 6, totalSamples = 96000, totalDurationMs = 6000))
            }
        }
        assertEquals(listOf(2, 5), error.missingSequences())
        assertFalse(error.isPermanent)
    }

    @Test
    fun `permanent vs retryable code classification`() {
        for (code in ApiError.PERMANENT_CODES) {
            assertTrue(code, ApiError(400, code, "").isPermanent)
        }
        for (code in listOf("rate_limited", "sequence_gap", "internal", "unauthorized", "recording_not_found")) {
            assertFalse(code, ApiError(500, code, "").isPermanent)
        }
        assertEquals(
            setOf("validation_failed", "conflict_checksum", "conflict_payload"),
            ApiError.PERMANENT_CODES,
        )
    }

    @Test
    fun `recording create request wire format mirrors openapi`() {
        val json = Json {
            ignoreUnknownKeys = true
            explicitNulls = false
            encodeDefaults = true
        }
        val wire = json.encodeToString(
            RecordingCreateRequest.serializer(),
            RecordingCreateRequest(recordingId = "0d7f4a1e-1111-4222-8333-444455556666"),
        )
        assertTrue(wire.contains("\"protocol_version\":\"ingest-v1\""))
        assertTrue(wire.contains("\"codec\":\"pcm_s16le\""))
        assertTrue(wire.contains("\"sample_rate\":16000"))
        assertTrue(wire.contains("\"channels\":1"))
        assertFalse("recorded_at null 时省略", wire.contains("recorded_at"))
    }
}
