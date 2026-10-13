// record-sim：ingest-v1 契约客户端验证器 + 录音模拟器。
//
// 生成确定性 PCM（16 kHz / mono / s16le）写入本地 spool，按 durable ACK 规则
// 释放（收到 HTTP 200 且 durable:true 才删除块文件），再按场景演练契约幂等
// 矩阵：normal / offline-recover / crash-restart / duplicate / bad-checksum /
// gap / integrity；--fault 复现服务端故障注入后的客户端恢复路径。
// 所有观察到的错误码写入 --report-json 报告。
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

const (
	protocolVersion = "ingest-v1"
	sampleRate      = 16000
	toneHz          = 440.0
	toneAmp         = 1000.0
	maxAttempts     = 6
)

// ------------------------------------------------------------------ options

type options struct {
	server           string
	pairCode         string
	deviceToken      string
	recordingID      string
	duration         time.Duration
	chunkSeconds     int
	pattern          string
	spool            string
	scenario         string
	offlineFor       time.Duration
	crashAfterChunks int
	fault            string
	reportPath       string
}

type report struct {
	RecordingID        string   `json:"recording_id"`
	Scenario           string   `json:"scenario"`
	ChunksGenerated    int      `json:"chunks_generated"`
	ChunksUploaded     int      `json:"chunks_uploaded"`
	Duplicates         int      `json:"duplicates"`
	Retries            int      `json:"retries"`
	ExpectedErrorCodes []string `json:"expected_error_codes"`
	MissingSequences   []int64  `json:"missing_sequences"`
	CoverageOK         bool     `json:"coverage_ok"`
	State              string   `json:"state"`
}

func main() {
	opt := &options{}
	flag.StringVar(&opt.server, "server", "", "ingest 服务基址（必填）")
	flag.StringVar(&opt.pairCode, "pair-code", "", "可选：先配对换取 device token（一次性，续传时复用 spool 凭据）")
	flag.StringVar(&opt.deviceToken, "device-token", "", "已有设备 token（跳过配对）")
	flag.StringVar(&opt.recordingID, "recording-id", "", "录音 ID（默认 UUIDv4；崩溃续传时自动沿用 spool 中未完成录音）")
	flag.DurationVar(&opt.duration, "duration", 120*time.Second, "录音总时长")
	flag.IntVar(&opt.chunkSeconds, "chunk-seconds", 30, "每块秒数（受契约 1000–90000ms 约束）")
	flag.StringVar(&opt.pattern, "pattern", "sine", "波形：sine|silence")
	flag.StringVar(&opt.spool, "spool", "data/sim-spool", "spool 根目录")
	flag.StringVar(&opt.scenario, "scenario", "normal",
		"场景：normal|offline-recover|crash-restart|duplicate|bad-checksum|gap|integrity")
	flag.DurationVar(&opt.offlineFor, "offline-for", 20*time.Second, "offline-recover 场景的离线时长")
	flag.IntVar(&opt.crashAfterChunks, "crash-after-chunks", 3, "crash-restart 场景：多少个 durable ack 后模拟崩溃")
	flag.StringVar(&opt.fault, "fault", "", "开发故障注入头：r2-after-write|d1-manifest|d1-event|response-drop（重试时不携带）")
	flag.StringVar(&opt.reportPath, "report-json", "", "报告输出路径")
	flag.Parse()

	if opt.server == "" {
		fmt.Fprintln(os.Stderr, "record-sim: --server 必填")
		os.Exit(2)
	}
	switch opt.pattern {
	case "sine", "silence":
	default:
		fmt.Fprintln(os.Stderr, "record-sim: --pattern 只支持 sine|silence")
		os.Exit(2)
	}
	switch opt.scenario {
	case "normal", "offline-recover", "crash-restart", "duplicate", "bad-checksum", "gap", "integrity":
	default:
		fmt.Fprintln(os.Stderr, "record-sim: 未知场景 "+opt.scenario)
		os.Exit(2)
	}
	switch opt.fault {
	case "", "r2-after-write", "d1-manifest", "d1-event", "response-drop":
	default:
		fmt.Fprintln(os.Stderr, "record-sim: 未知故障注入 "+opt.fault)
		os.Exit(2)
	}
	if opt.scenario == "crash-restart" && opt.crashAfterChunks < 1 {
		fmt.Fprintln(os.Stderr, "record-sim: --crash-after-chunks 至少为 1")
		os.Exit(2)
	}

	rep := &report{
		Scenario:           opt.scenario,
		ExpectedErrorCodes: []string{},
		MissingSequences:   []int64{},
		State:              "unknown",
	}
	code := execute(opt, rep)
	if err := writeJSONFile(opt.reportPath, rep); err != nil {
		fmt.Fprintln(os.Stderr, "record-sim: 写报告失败:", err)
	}
	out, _ := json.Marshal(rep)
	fmt.Println("report " + string(out))
	if code != 0 {
		os.Exit(code)
	}
}

func execute(opt *options, rep *report) int {
	s, err := newSim(opt, rep)
	if err != nil {
		fmt.Fprintln(os.Stderr, "record-sim:", err)
		return 2
	}
	var runErr error
	switch opt.scenario {
	case "normal":
		runErr = s.runNormal()
	case "offline-recover":
		runErr = s.runOfflineRecover()
	case "crash-restart":
		return s.runCrashRestart()
	case "duplicate":
		runErr = s.runDuplicate()
	case "bad-checksum":
		runErr = s.runBadChecksum()
	case "gap":
		runErr = s.runGap()
	case "integrity":
		runErr = s.runIntegrity()
	}
	if runErr != nil {
		fmt.Fprintln(os.Stderr, "record-sim:", runErr)
		return 1
	}
	return 0
}

// ------------------------------------------------------------------- spool

type chunkSpec struct {
	Seq          int64 `json:"seq"`
	SampleOffset int64 `json:"sample_offset"`
	SampleCount  int64 `json:"sample_count"`
	DurationMS   int64 `json:"duration_ms"`
}

type chunkRec struct {
	SHA256       string `json:"sha256"`
	Bytes        int64  `json:"bytes"`
	SampleOffset int64  `json:"sample_offset"`
	SampleCount  int64  `json:"sample_count"`
	DurationMS   int64  `json:"duration_ms"`
	Uploaded     bool   `json:"uploaded"`
}

type spoolState struct {
	RecordingID     string               `json:"recording_id"`
	DeviceID        string               `json:"device_id,omitempty"`
	DeviceToken     string               `json:"device_token,omitempty"`
	RecordedAt      string               `json:"recorded_at,omitempty"`
	ChunkSeconds    int                  `json:"chunk_seconds"`
	Pattern         string               `json:"pattern"`
	DurationS       int64                `json:"duration_s"`
	TotalSamples    int64                `json:"total_samples"`
	StitchedSHA256  string               `json:"stitched_sha256"`
	Chunks          map[string]*chunkRec `json:"chunks"`
	UploadWatermark int64                `json:"upload_watermark"`
	Crashed         bool                 `json:"crashed"`
	Finished        bool                 `json:"finished"`
}

type sim struct {
	opt   *options
	rep   *report
	cl    *client
	dir   string // spool/<recording_id>
	st    *spoolState
	specs []chunkSpec
}

func newSim(opt *options, rep *report) (*sim, error) {
	if err := os.MkdirAll(opt.spool, 0o755); err != nil {
		return nil, err
	}
	rid := opt.recordingID
	if rid == "" {
		resume, err := findResumable(opt.spool)
		if err != nil {
			return nil, err
		}
		if resume != "" {
			rid = resume
		} else {
			rid, err = newUUIDv4()
			if err != nil {
				return nil, err
			}
		}
	}
	dir := filepath.Join(opt.spool, rid)
	var st *spoolState
	if b, err := os.ReadFile(filepath.Join(dir, "state.json")); err == nil {
		st = &spoolState{}
		if err := json.Unmarshal(b, st); err != nil {
			return nil, fmt.Errorf("解析 %s: %w", filepath.Join(dir, "state.json"), err)
		}
	}
	if st == nil {
		st = &spoolState{
			RecordingID:  rid,
			ChunkSeconds: opt.chunkSeconds,
			Pattern:      opt.pattern,
			DurationS:    int64(opt.duration.Seconds()),
		}
	}
	if st.Chunks == nil {
		st.Chunks = map[string]*chunkRec{}
	}
	totalSamples := st.DurationS * sampleRate
	if totalSamples < sampleRate {
		return nil, errors.New("--duration 至少 1s")
	}
	specs := computeLayout(totalSamples, st.ChunkSeconds)
	for _, spec := range specs {
		if spec.DurationMS < 1000 || spec.DurationMS > 90000 {
			return nil, fmt.Errorf("块时长越界（契约 1000–90000ms）：duration=%ds chunk-seconds=%d", st.DurationS, st.ChunkSeconds)
		}
	}

	s := &sim{
		opt:   opt,
		rep:   rep,
		cl:    &client{base: opt.server, hc: &http.Client{Timeout: 30 * time.Second}},
		dir:   dir,
		st:    st,
		specs: specs,
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, err
	}
	rep.RecordingID = rid
	rep.ChunksGenerated = len(specs)
	if err := s.ensureFiles(); err != nil {
		return nil, err
	}
	if err := s.saveState(); err != nil {
		return nil, err
	}
	return s, nil
}

// findResumable 在 spool 根目录下找唯一未完成的录音（crash-restart 续传用）。
func findResumable(base string) (string, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", err
	}
	found := ""
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(base, e.Name(), "state.json"))
		if err != nil {
			continue
		}
		var st spoolState
		if json.Unmarshal(b, &st) != nil || st.Finished {
			continue
		}
		if found != "" {
			return "", errors.New("spool 中存在多个未完成录音，请用 --recording-id 指定")
		}
		found = e.Name()
	}
	return found, nil
}

// computeLayout 把总采样数切成块；尾块不足 1s 时并入前一块（契约 1000–90000ms）。
func computeLayout(totalSamples int64, chunkSeconds int) []chunkSpec {
	perChunk := int64(chunkSeconds) * sampleRate
	specs := []chunkSpec{}
	for start := int64(0); start < totalSamples; {
		end := start + perChunk
		if end > totalSamples {
			end = totalSamples
		}
		specs = append(specs, chunkSpec{
			Seq:          int64(len(specs)),
			SampleOffset: start,
			SampleCount:  end - start,
			DurationMS:   (end - start) / 16,
		})
		start = end
	}
	if n := len(specs); n > 1 && specs[n-1].DurationMS < 1000 {
		prev := &specs[n-2]
		prev.SampleCount = totalSamples - prev.SampleOffset
		prev.DurationMS = prev.SampleCount / 16
		specs = specs[:n-1]
	}
	return specs
}

// ensureFiles 保证每个未上传块都在磁盘上（崩溃窗口内文件缺失/损坏则确定性重建）。
func (s *sim) ensureFiles() error {
	stitched := sha256.New()
	for _, spec := range s.specs {
		key := strconv.FormatInt(spec.Seq, 10)
		rec := s.st.Chunks[key]
		layoutOK := rec != nil && rec.SampleOffset == spec.SampleOffset &&
			rec.SampleCount == spec.SampleCount && rec.DurationMS == spec.DurationMS
		if !layoutOK {
			if err := s.regenerate(spec); err != nil {
				return err
			}
		} else if !rec.Uploaded {
			body, err := os.ReadFile(s.chunkPath(spec.Seq))
			if err != nil || int64(len(body)) != rec.Bytes || sha256Hex(body) != rec.SHA256 {
				if err := s.regenerate(spec); err != nil {
					return err
				}
			}
		}
		stitched.Write(chunkBytes(s.st.Pattern, spec.SampleOffset, spec.SampleCount))
	}
	s.st.StitchedSHA256 = hex.EncodeToString(stitched.Sum(nil))
	s.st.TotalSamples = s.specs[len(s.specs)-1].SampleOffset + s.specs[len(s.specs)-1].SampleCount
	return nil
}

func (s *sim) regenerate(spec chunkSpec) error {
	body := chunkBytes(s.st.Pattern, spec.SampleOffset, spec.SampleCount)
	if err := writeFileSync(s.chunkPath(spec.Seq), body); err != nil {
		return err
	}
	s.st.Chunks[strconv.FormatInt(spec.Seq, 10)] = &chunkRec{
		SHA256:       sha256Hex(body),
		Bytes:        int64(len(body)),
		SampleOffset: spec.SampleOffset,
		SampleCount:  spec.SampleCount,
		DurationMS:   spec.DurationMS,
	}
	return nil
}

// chunkBytes 生成确定性 PCM：sine 为 440 Hz / 幅值 1000（与契约向量同公式），
// silence 为全零。
func chunkBytes(pattern string, offset, count int64) []byte {
	buf := make([]byte, count*2)
	for i := range count {
		var v int16
		if pattern == "sine" {
			v = int16(math.Round(toneAmp * math.Sin(2*math.Pi*toneHz*float64(offset+i)/sampleRate)))
		}
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(v))
	}
	return buf
}

// corruptBytes 生成与正常块同长度的确定性"不同内容"（bad-checksum 冲突探测用）。
func corruptBytes(offset, count int64) []byte {
	buf := make([]byte, count*2)
	for i := range count {
		v := int16(math.Round((toneAmp - 1) * math.Sin(2*math.Pi*toneHz*float64(offset+i)/sampleRate)))
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(v))
	}
	return buf
}

func (s *sim) chunkPath(seq int64) string {
	return filepath.Join(s.dir, fmt.Sprintf("chunk-%04d.pcm", seq))
}

func (s *sim) countUploaded() int {
	n := 0
	for _, rec := range s.st.Chunks {
		if rec.Uploaded {
			n++
		}
	}
	return n
}

func (s *sim) saveState() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, "state.json.tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "state.json"))
}

func writeFileSync(path string, body []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func writeJSONFile(path string, v any) error {
	if path == "" {
		return nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ------------------------------------------------------------------- client

type apiErr struct {
	status  int
	code    string
	message string
	missing []int64
}

func (e *apiErr) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.message != "" {
		return fmt.Sprintf("HTTP %d %s: %s", e.status, e.code, e.message)
	}
	return fmt.Sprintf("HTTP %d %s", e.status, e.code)
}

func parseAPIErr(status int, body []byte) *apiErr {
	var eb struct {
		Error struct {
			Code    string          `json:"code"`
			Message string          `json:"message"`
			Details json.RawMessage `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &eb)
	ae := &apiErr{status: status, code: eb.Error.Code, message: eb.Error.Message}
	if len(eb.Error.Details) > 0 {
		var d struct {
			MissingSequences []int64 `json:"missing_sequences"`
		}
		_ = json.Unmarshal(eb.Error.Details, &d)
		ae.missing = d.MissingSequences
	}
	return ae
}

type client struct {
	base  string
	token string
	hc    *http.Client
}

// do 发送一个 /v1 请求；所有请求都带 X-Ingest-Version，token 非空时带 Bearer。
// 返回 (status, body, apiErr, transportErr)。
func (c *client) do(method, path string, body []byte, hdrs map[string]string) (int, []byte, *apiErr, error) {
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("X-Ingest-Version", protocolVersion)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, b, parseAPIErr(resp.StatusCode, b), nil
	}
	return resp.StatusCode, b, nil, nil
}

var jsonHeaders = map[string]string{"Content-Type": "application/json"}

type chunkMeta struct {
	SampleOffset  int64   `json:"sample_offset"`
	SampleCount   int64   `json:"sample_count"`
	DurationMS    int64   `json:"duration_ms"`
	ContentSHA256 string  `json:"content_sha256"`
	RecordedAt    *string `json:"recorded_at"`
}

type chunkResp struct {
	Seq       int64  `json:"seq"`
	State     string `json:"state"`
	Durable   bool   `json:"durable"`
	Duplicate bool   `json:"duplicate"`
}

type createRecBody struct {
	ProtocolVersion string  `json:"protocol_version"`
	RecordingID     string  `json:"recording_id"`
	Codec           string  `json:"codec"`
	SampleRate      int     `json:"sample_rate"`
	Channels        int     `json:"channels"`
	RecordedAt      string  `json:"recorded_at"`
	Timezone        *string `json:"timezone"`
}

type finishBody struct {
	ProtocolVersion string `json:"protocol_version"`
	TotalChunks     int    `json:"total_chunks"`
	TotalSamples    int64  `json:"total_samples"`
	TotalDurationMS int64  `json:"total_duration_ms"`
}

type recordingDetail struct {
	State        string `json:"state"`
	TotalChunks  int64  `json:"total_chunks"`
	TotalSamples int64  `json:"total_samples"`
	Chunks       []struct {
		Seq           int64  `json:"seq"`
		Bytes         int64  `json:"bytes"`
		SampleOffset  int64  `json:"sample_offset"`
		SampleCount   int64  `json:"sample_count"`
		ContentSHA256 string `json:"content_sha256"`
	} `json:"chunks"`
}

// observe 记录观察到的错误码（同码去重，保留首次出现顺序）。
func (s *sim) observe(ae *apiErr) {
	if ae == nil || ae.code == "" {
		return
	}
	for _, c := range s.rep.ExpectedErrorCodes {
		if c == ae.code {
			return
		}
	}
	s.rep.ExpectedErrorCodes = append(s.rep.ExpectedErrorCodes, ae.code)
}

func backoff(attempt int) time.Duration {
	d := 200 * time.Millisecond << attempt
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

// -------------------------------------------------------------- API steps

func (s *sim) ensureToken() error {
	if s.st.DeviceToken != "" {
		s.cl.token = s.st.DeviceToken
		return nil
	}
	if s.opt.deviceToken != "" {
		s.st.DeviceToken = s.opt.deviceToken
		s.cl.token = s.opt.deviceToken
		return s.saveState()
	}
	if s.opt.pairCode == "" {
		return errors.New("需要 --pair-code 或 --device-token（或 spool 中已有凭据）")
	}
	body, _ := json.Marshal(map[string]any{
		"protocol_version": protocolVersion,
		"pairing_code":     s.opt.pairCode,
		"device_name":      "record-sim",
	})
	status, rb, ae, terr := s.cl.do("POST", "/v1/devices/pair", body, jsonHeaders)
	if terr != nil {
		return fmt.Errorf("pair: %w", terr)
	}
	if status != 200 {
		s.observe(ae)
		return fmt.Errorf("pair 失败: %s", ae)
	}
	var pr struct {
		DeviceID    string `json:"device_id"`
		DeviceToken string `json:"device_token"`
	}
	if err := json.Unmarshal(rb, &pr); err != nil || pr.DeviceToken == "" {
		return errors.New("pair 响应缺少 device_token")
	}
	s.st.DeviceID = pr.DeviceID
	s.st.DeviceToken = pr.DeviceToken
	s.cl.token = pr.DeviceToken
	fmt.Println("device_token=" + pr.DeviceToken)
	return s.saveState()
}

func (s *sim) createRecording() error {
	if s.st.RecordedAt == "" {
		s.st.RecordedAt = time.Now().UTC().Format(time.RFC3339)
		if err := s.saveState(); err != nil {
			return err
		}
	}
	// recorded_at 持久化在 spool state 里：崩溃续传重放必须与首次逐字节等价，
	// 否则契约按"同 ID 不同字段"判 409 conflict_payload。
	body, _ := json.Marshal(createRecBody{
		ProtocolVersion: protocolVersion,
		RecordingID:     s.st.RecordingID,
		Codec:           "pcm_s16le",
		SampleRate:      sampleRate,
		Channels:        1,
		RecordedAt:      s.st.RecordedAt,
	})
	status, _, ae, terr := s.cl.do("POST", "/v1/recordings", body, jsonHeaders)
	if terr != nil {
		return fmt.Errorf("create recording: %w", terr)
	}
	if status == 201 || status == 200 {
		fmt.Printf("recording %s created (HTTP %d)\n", s.st.RecordingID, status)
		return nil
	}
	s.observe(ae)
	return fmt.Errorf("create recording: %s", ae)
}

// attemptPUTRaw 发送一次 PUT chunk；shaHex 允许故意写错（bad-checksum 探测）。
func (s *sim) attemptPUTRaw(seq int64, body []byte, shaHex string, withFault bool) (int, []byte, *apiErr, error) {
	rec := s.st.Chunks[strconv.FormatInt(seq, 10)]
	meta, _ := json.Marshal(chunkMeta{
		SampleOffset:  rec.SampleOffset,
		SampleCount:   rec.SampleCount,
		DurationMS:    rec.DurationMS,
		ContentSHA256: shaHex,
	})
	hdrs := map[string]string{
		"Content-Type": "application/octet-stream",
		"X-Chunk-Meta": base64.RawURLEncoding.EncodeToString(meta),
	}
	// 故障注入只在首次尝试携带：契约要求客户端对 500 原样重试且重试恒安全，
	// 重试不再带 X-Debug-Fault 才能让服务端走正常持久化/重放路径。
	if withFault && s.opt.fault != "" {
		hdrs["X-Debug-Fault"] = s.opt.fault
	}
	return s.cl.do("PUT", fmt.Sprintf("/v1/recordings/%s/chunks/%d", s.st.RecordingID, seq), body, hdrs)
}

// uploadSeq 上传一个块，500/429/网络错误按契约原样重试（故障头仅首次）。
func (s *sim) uploadSeq(seq int64, rec *chunkRec, body []byte) (*chunkResp, error) {
	var lastErr error
	for attempt := range maxAttempts {
		status, rb, ae, terr := s.attemptPUTRaw(seq, body, rec.SHA256, attempt == 0)
		switch {
		case terr != nil:
			lastErr = terr
		case status == 200:
			var cr chunkResp
			if err := json.Unmarshal(rb, &cr); err != nil {
				return nil, err
			}
			if !cr.Durable {
				lastErr = fmt.Errorf("seq %d: 200 响应缺少 durable:true", seq)
				break
			}
			return &cr, nil
		case ae != nil && (ae.status == 429 || ae.status >= 500):
			lastErr = ae
		default:
			if ae == nil {
				ae = &apiErr{status: status, code: fmt.Sprintf("http_%d", status)}
			}
			return nil, ae
		}
		s.rep.Retries++
		time.Sleep(backoff(attempt))
	}
	return nil, fmt.Errorf("chunk %d 重试耗尽: %w", seq, lastErr)
}

func (s *sim) markUploaded(spec chunkSpec) error {
	rec := s.st.Chunks[strconv.FormatInt(spec.Seq, 10)]
	if err := os.Remove(s.chunkPath(spec.Seq)); err != nil && !os.IsNotExist(err) {
		return err
	}
	rec.Uploaded = true
	for s.st.Chunks[strconv.FormatInt(s.st.UploadWatermark+1, 10)] != nil &&
		s.st.Chunks[strconv.FormatInt(s.st.UploadWatermark+1, 10)].Uploaded {
		s.st.UploadWatermark++
	}
	return s.saveState()
}

// uploadAndMark 读取 spool 块并上传；响应丢失后的重放命中 duplicate:true 计入
// retries 而非 duplicates（duplicates 只统计 duplicate 场景的显式重传）。
func (s *sim) uploadAndMark(spec chunkSpec) error {
	rec := s.st.Chunks[strconv.FormatInt(spec.Seq, 10)]
	if rec.Uploaded {
		return nil
	}
	body, err := os.ReadFile(s.chunkPath(spec.Seq))
	if err != nil {
		return fmt.Errorf("读 spool 块 %d: %w", spec.Seq, err)
	}
	cr, err := s.uploadSeq(spec.Seq, rec, body)
	if err != nil {
		return err
	}
	if cr.Duplicate {
		fmt.Printf("committed seq=%d duplicate=true (replay)\n", spec.Seq)
	} else {
		fmt.Printf("committed seq=%d durable=true\n", spec.Seq)
	}
	return s.markUploaded(spec)
}

func (s *sim) uploadAll(skip map[int64]bool) error {
	for _, spec := range s.specs {
		if skip[spec.Seq] {
			continue
		}
		if err := s.uploadAndMark(spec); err != nil {
			return err
		}
	}
	return nil
}

// finish 调用 finish；成功返回 state，缺口返回 missing（同时记录错误码）。
func (s *sim) finish() (string, []int64, error) {
	var totalSamples, totalMS int64
	for _, spec := range s.specs {
		totalSamples += spec.SampleCount
		totalMS += spec.DurationMS
	}
	body, _ := json.Marshal(finishBody{
		ProtocolVersion: protocolVersion,
		TotalChunks:     len(s.specs),
		TotalSamples:    totalSamples,
		TotalDurationMS: totalMS,
	})
	status, rb, ae, terr := s.cl.do("POST", fmt.Sprintf("/v1/recordings/%s/finish", s.st.RecordingID), body, jsonHeaders)
	if terr != nil {
		return "", nil, fmt.Errorf("finish: %w", terr)
	}
	if status == 200 {
		var fr struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(rb, &fr)
		fmt.Printf("finish state=%s\n", fr.State)
		return fr.State, nil, nil
	}
	if ae != nil {
		s.observe(ae)
		if ae.code == "sequence_gap" {
			s.rep.MissingSequences = append([]int64{}, ae.missing...)
			return "", ae.missing, nil
		}
		return "", nil, fmt.Errorf("finish: %s", ae)
	}
	return "", nil, fmt.Errorf("finish: HTTP %d", status)
}

func (s *sim) getDetail() (*recordingDetail, error) {
	status, rb, ae, terr := s.cl.do("GET", fmt.Sprintf("/v1/recordings/%s", s.st.RecordingID), nil, nil)
	if terr != nil {
		return nil, fmt.Errorf("GET recording: %w", terr)
	}
	if status != 200 {
		if ae != nil {
			s.observe(ae)
			return nil, fmt.Errorf("GET recording: %s", ae)
		}
		return nil, fmt.Errorf("GET recording: HTTP %d", status)
	}
	var d recordingDetail
	if err := json.Unmarshal(rb, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// finalize 汇总报告；complete 时清理 spool（块文件已逐个删除，这里删 state）。
func (s *sim) finalize(state string, detail *recordingDetail) {
	s.rep.State = state
	s.rep.ChunksUploaded = s.countUploaded()
	ok := state == "complete" && s.rep.ChunksUploaded == len(s.specs)
	if detail != nil {
		ok = ok && detail.State == "complete" &&
			detail.TotalChunks == int64(len(s.specs)) &&
			int64(len(detail.Chunks)) == int64(len(s.specs))
	}
	s.rep.CoverageOK = ok
	if state == "complete" {
		s.st.Finished = true
		_ = s.saveState()
		_ = os.RemoveAll(s.dir)
	}
}

// --------------------------------------------------------------- scenarios

func (s *sim) runNormal() error {
	if err := s.ensureToken(); err != nil {
		return err
	}
	if err := s.createRecording(); err != nil {
		return err
	}
	if err := s.uploadAll(nil); err != nil {
		return err
	}
	state, _, err := s.finish()
	if err != nil {
		return err
	}
	detail, err := s.getDetail()
	if err != nil {
		return err
	}
	s.finalize(state, detail)
	return nil
}

func (s *sim) runOfflineRecover() error {
	if err := s.ensureToken(); err != nil {
		return err
	}
	if err := s.createRecording(); err != nil {
		return err
	}
	fmt.Printf("offline: 暂不上传，等待 %s\n", s.opt.offlineFor)
	time.Sleep(s.opt.offlineFor)
	fmt.Println("offline: 恢复，开始补传")
	if err := s.uploadAll(nil); err != nil {
		return err
	}
	state, _, err := s.finish()
	if err != nil {
		return err
	}
	detail, err := s.getDetail()
	if err != nil {
		return err
	}
	s.finalize(state, detail)
	return nil
}

// runCrashRestart 上传 crashAfterChunks 个 durable ack 后返回 1（模拟进程死亡）；
// 再次运行时从 state.json 续传（Crashed=true 后不再触发崩溃）。
func (s *sim) runCrashRestart() int {
	if err := s.ensureToken(); err != nil {
		fmt.Fprintln(os.Stderr, "record-sim:", err)
		return 2
	}
	if err := s.createRecording(); err != nil {
		fmt.Fprintln(os.Stderr, "record-sim:", err)
		return 1
	}
	acked := 0
	for _, spec := range s.specs {
		if s.st.Chunks[strconv.FormatInt(spec.Seq, 10)].Uploaded {
			continue
		}
		if err := s.uploadAndMark(spec); err != nil {
			fmt.Fprintln(os.Stderr, "record-sim:", err)
			return 1
		}
		if !s.st.Crashed {
			acked++
			if acked >= s.opt.crashAfterChunks {
				s.st.Crashed = true
				_ = s.saveState()
				s.rep.State = "crashed"
				s.rep.ChunksUploaded = s.countUploaded()
				s.rep.CoverageOK = false
				fmt.Fprintf(os.Stderr,
					"record-sim: crash-restart: %d 个 durable ack 后模拟崩溃（原样重跑即可续传）\n", acked)
				return 1
			}
		}
	}
	state, _, err := s.finish()
	if err != nil {
		fmt.Fprintln(os.Stderr, "record-sim:", err)
		return 1
	}
	detail, err := s.getDetail()
	if err != nil {
		fmt.Fprintln(os.Stderr, "record-sim:", err)
		return 1
	}
	s.finalize(state, detail)
	return 0
}

func (s *sim) runDuplicate() error {
	if err := s.ensureToken(); err != nil {
		return err
	}
	if err := s.createRecording(); err != nil {
		return err
	}
	if err := s.uploadAll(nil); err != nil {
		return err
	}
	spec0 := s.specs[0]
	rec := s.st.Chunks[strconv.FormatInt(spec0.Seq, 10)]
	body := chunkBytes(s.st.Pattern, spec0.SampleOffset, spec0.SampleCount)
	cr, err := s.uploadSeq(spec0.Seq, rec, body)
	if err != nil {
		return err
	}
	if !cr.Duplicate {
		return errors.New("重传首块未返回 duplicate:true")
	}
	s.rep.Duplicates++
	fmt.Println("duplicate probe: duplicate=true")
	state, _, err := s.finish()
	if err != nil {
		return err
	}
	detail, err := s.getDetail()
	if err != nil {
		return err
	}
	s.finalize(state, detail)
	return nil
}

func (s *sim) runBadChecksum() error {
	if err := s.ensureToken(); err != nil {
		return err
	}
	if err := s.createRecording(); err != nil {
		return err
	}
	spec0 := s.specs[0]
	good := chunkBytes(s.st.Pattern, spec0.SampleOffset, spec0.SampleCount)

	// 1) meta sha 与 body 不符 → validation_failed
	_, _, ae, terr := s.attemptPUTRaw(spec0.Seq, good, sha256Hex([]byte("record-sim bad-checksum probe")), false)
	if terr != nil {
		return terr
	}
	if ae == nil {
		return errors.New("错误 content_sha256 的 PUT 未被拒绝")
	}
	s.observe(ae)
	if ae.code != "validation_failed" {
		return fmt.Errorf("期望 validation_failed，得到 %s", ae)
	}
	fmt.Println("bad-checksum probe 1: validation_failed")

	// 2) 正常提交 seq 0
	rec := s.st.Chunks[strconv.FormatInt(spec0.Seq, 10)]
	if _, err := s.uploadSeq(spec0.Seq, rec, good); err != nil {
		return err
	}
	if err := s.markUploaded(spec0); err != nil {
		return err
	}
	fmt.Printf("committed seq=%d durable=true\n", spec0.Seq)

	// 3) 同 seq 不同内容（sha 自洽）→ conflict_checksum
	bad := corruptBytes(spec0.SampleOffset, spec0.SampleCount)
	_, _, ae2, terr := s.attemptPUTRaw(spec0.Seq, bad, sha256Hex(bad), false)
	if terr != nil {
		return terr
	}
	if ae2 == nil {
		return errors.New("同 seq 不同内容的 PUT 未被拒绝")
	}
	s.observe(ae2)
	if ae2.code != "conflict_checksum" {
		return fmt.Errorf("期望 conflict_checksum，得到 %s", ae2)
	}
	fmt.Println("bad-checksum probe 2: conflict_checksum")

	// 4) 其余块 + finish
	if err := s.uploadAll(nil); err != nil {
		return err
	}
	state, _, err := s.finish()
	if err != nil {
		return err
	}
	detail, err := s.getDetail()
	if err != nil {
		return err
	}
	s.finalize(state, detail)
	return nil
}

func (s *sim) runGap() error {
	if len(s.specs) < 4 {
		return fmt.Errorf("gap 场景需要至少 4 块（跳过 seq 2），当前 %d 块", len(s.specs))
	}
	if err := s.ensureToken(); err != nil {
		return err
	}
	if err := s.createRecording(); err != nil {
		return err
	}
	if err := s.uploadAll(map[int64]bool{2: true}); err != nil {
		return err
	}
	state, _, err := s.finish()
	if err != nil {
		return err
	}
	if state != "" {
		return errors.New("gap 场景意外 complete（缺失序列绝不能 complete）")
	}
	found := false
	for _, m := range s.rep.MissingSequences {
		if m == 2 {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("missing_sequences 未包含 2: %v", s.rep.MissingSequences)
	}
	fmt.Printf("gap probe: sequence_gap missing=%v\n", s.rep.MissingSequences)
	detail, err := s.getDetail()
	if err != nil {
		return err
	}
	if detail.State != "recording" {
		return fmt.Errorf("gap 后云端 state=%q，期望 recording", detail.State)
	}
	s.rep.State = detail.State
	s.rep.ChunksUploaded = s.countUploaded()
	s.rep.CoverageOK = false
	return nil
}

func (s *sim) runIntegrity() error {
	if err := s.ensureToken(); err != nil {
		return err
	}
	if err := s.createRecording(); err != nil {
		return err
	}
	if err := s.uploadAll(nil); err != nil {
		return err
	}
	state, _, err := s.finish()
	if err != nil {
		return err
	}
	detail, err := s.getDetail()
	if err != nil {
		return err
	}
	var totalSamples int64
	for _, spec := range s.specs {
		totalSamples += spec.SampleCount
	}
	if detail.TotalSamples != totalSamples {
		return fmt.Errorf("total_samples=%d，期望 %d", detail.TotalSamples, totalSamples)
	}
	if detail.TotalChunks != int64(len(s.specs)) {
		return fmt.Errorf("total_chunks=%d，期望 %d", detail.TotalChunks, len(s.specs))
	}
	chunks := append([]struct {
		Seq           int64  `json:"seq"`
		Bytes         int64  `json:"bytes"`
		SampleOffset  int64  `json:"sample_offset"`
		SampleCount   int64  `json:"sample_count"`
		ContentSHA256 string `json:"content_sha256"`
	}{}, detail.Chunks...)
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].Seq < chunks[j].Seq })
	prevEnd := int64(0)
	for i, c := range chunks {
		spec := s.specs[i]
		if c.Seq != spec.Seq {
			return fmt.Errorf("chunk %d: seq=%d，期望 %d（seq 集合应为 0..n-1）", i, c.Seq, spec.Seq)
		}
		if c.SampleOffset != spec.SampleOffset || c.SampleCount != spec.SampleCount {
			return fmt.Errorf("chunk %d: offset/count=(%d,%d)，期望 (%d,%d)",
				c.Seq, c.SampleOffset, c.SampleCount, spec.SampleOffset, spec.SampleCount)
		}
		if i > 0 && c.SampleOffset != prevEnd {
			return fmt.Errorf("chunk %d: offset %d 不连续（前块末尾 %d）", c.Seq, c.SampleOffset, prevEnd)
		}
		prevEnd = c.SampleOffset + c.SampleCount
		if c.Bytes != c.SampleCount*2 {
			return fmt.Errorf("chunk %d: bytes=%d，期望 %d", c.Seq, c.Bytes, c.SampleCount*2)
		}
		// stitched 与逐块列表一致：重算该块内容 sha 必须等于服务端登记值
		if want := sha256Hex(chunkBytes(s.st.Pattern, spec.SampleOffset, spec.SampleCount)); c.ContentSHA256 != want {
			return fmt.Errorf("chunk %d: content_sha256=%s，期望 %s", c.Seq, c.ContentSHA256, want)
		}
	}
	stitched := sha256.New()
	for _, spec := range s.specs {
		stitched.Write(chunkBytes(s.st.Pattern, spec.SampleOffset, spec.SampleCount))
	}
	if got := hex.EncodeToString(stitched.Sum(nil)); got != s.st.StitchedSHA256 {
		return fmt.Errorf("stitched sha256=%s，与逐块拼接基准 %s 不一致", got, s.st.StitchedSHA256)
	}
	fmt.Printf("integrity: %d 块 offset 连续、sha256 全部一致（stitched=%s）\n", len(chunks), s.st.StitchedSHA256)
	s.finalize(state, detail)
	return nil
}
