package profile

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/migrations"
	"recordingmcp/internal/retention"
)

const worldlineBytesPerMS = 32 // ingest-v1: 16 kHz mono signed 16-bit PCM.

type WorldlinePrepareOptions struct {
	LibraryDir string
	AudioRoot  string
	Timezone   string
}

type WorldlinePrepareReport struct {
	Recordings  int                  `json:"recordings"`
	TimesSet    int                  `json:"times_set"`
	AudioReady  int                  `json:"audio_ready"`
	Unavailable int                  `json:"audio_unavailable"`
	Issues      int                  `json:"issues"`
	Manifest    string               `json:"manifest"`
	Entries     []WorldlineRecording `json:"-"`
}

type WorldlineRecording struct {
	RecordingID           string   `json:"recording_id"`
	Filename              string   `json:"filename"`
	StartedAt             string   `json:"start_at"`
	Basis                 string   `json:"basis"`
	OriginalMtime         string   `json:"original_mtime,omitempty"`
	EndBasis              string   `json:"end_basis"`
	EndAt                 string   `json:"end_at,omitempty"`
	AudioStatus           string   `json:"audio_status"`
	OriginalAudioVerified bool     `json:"original_audio_verified"`
	AudioPath             string   `json:"audio_path,omitempty"`
	Issues                []string `json:"issues,omitempty"`
}

type worldlineManifest struct {
	Version    int                  `json:"version"`
	PreparedAt string               `json:"prepared_at"`
	Timezone   string               `json:"timezone"`
	Recordings []WorldlineRecording `json:"recordings"`
}

type runManifest struct {
	AudioFileName string       `json:"audio_file_name"`
	AudioSHA256   string       `json:"audio_sha256"`
	Duration      float64      `json:"duration_seconds"`
	Segments      []runSegment `json:"segments"`
}

type runSegment struct {
	Index   int    `json:"index"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	PCM     string `json:"pcm"`
}

type sourceRecording struct {
	id, conversationID, device, providerID, sourceAudioHash string
	durationMS                                              int64
	startedAt                                               sql.NullString
	timeKnown                                               int
}

type locatedRun struct {
	dir      string
	manifest runManifest
	start    time.Time
	filename string
}

// ParseWorldlineFilename accepts only YYYYMMDDHHmmss and PHYYMMDDHHmmss
// filename stems. PH's two-digit year is interpreted as 20YY.
func ParseWorldlineFilename(name string, loc *time.Location) (time.Time, error) {
	if loc == nil {
		return time.Time{}, errors.New("worldline: timezone is required")
	}
	base := filepath.Base(name)
	if base != name || base == "." || base == ".." {
		return time.Time{}, errors.New("worldline: filename must be a basename")
	}
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if strings.EqualFold(filepath.Ext(base), ".") || stem == base || strings.ContainsAny(stem, `/\\`) {
		return time.Time{}, fmt.Errorf("worldline: filename %q has no valid extension", name)
	}
	layout := "20060102150405"
	value := stem
	if strings.HasPrefix(stem, "PH") {
		if len(stem) != 14 {
			return time.Time{}, fmt.Errorf("worldline: invalid PH timestamp filename %q", name)
		}
		value = "20" + stem[2:]
	} else if len(stem) != 14 {
		return time.Time{}, fmt.Errorf("worldline: invalid timestamp filename %q", name)
	}
	t, err := time.ParseInLocation(layout, value, loc)
	if err != nil || t.Format(layout) != value {
		return time.Time{}, fmt.Errorf("worldline: invalid calendar timestamp filename %q", name)
	}
	return t, nil
}

// PrepareWorldline authenticates an existing profile and updates only its
// canonical database/audio/tasks. Library and original-audio trees are
// opened read-only and rejected if they contain symlinked source paths.
func (m *Manager) PrepareWorldline(ctx context.Context, profileName, password string, opts WorldlinePrepareOptions) (WorldlinePrepareReport, error) {
	if !m.Verify(profileName, password) {
		return WorldlinePrepareReport{}, errors.New("profile: invalid profile or password")
	}
	paths, err := m.Resolve(profileName)
	if err != nil {
		return WorldlinePrepareReport{}, err
	}
	return prepareWorldline(ctx, paths, opts)
}

func prepareWorldline(ctx context.Context, paths Paths, opts WorldlinePrepareOptions) (WorldlinePrepareReport, error) {
	var report WorldlinePrepareReport
	if strings.TrimSpace(opts.LibraryDir) == "" {
		return report, errors.New("worldline: --library-dir is required")
	}
	zone := strings.TrimSpace(opts.Timezone)
	if zone == "" {
		zone = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return report, fmt.Errorf("worldline: load timezone: %w", err)
	}
	library, err := realDirectoryPath(opts.LibraryDir)
	if err != nil {
		return report, fmt.Errorf("worldline: library directory: %w", err)
	}
	if err = rejectSymlinkComponents(library); err != nil {
		return report, fmt.Errorf("worldline: library path: %w", err)
	}
	var originals string
	if strings.TrimSpace(opts.AudioRoot) != "" {
		originals, err = realDirectoryPath(opts.AudioRoot)
		if err != nil {
			return report, fmt.Errorf("worldline: original audio root: %w", err)
		}
		if err = rejectSymlinkComponents(originals); err != nil {
			return report, fmt.Errorf("worldline: original audio path: %w", err)
		}
	}
	profileAudio, err := realDirectoryPath(paths.Audio)
	if err != nil {
		return report, fmt.Errorf("worldline: profile audio root: %w", err)
	}
	if err = rejectSymlinkComponents(profileAudio); err != nil {
		return report, fmt.Errorf("worldline: profile audio path: %w", err)
	}
	releaseAudio, err := retention.Lock(ctx, profileAudio, true)
	if err != nil {
		return report, fmt.Errorf("worldline: acquire profile audio lock: %w", err)
	}
	defer releaseAudio()
	if err = requireRegularNoSymlink(paths.Database); err != nil {
		return report, fmt.Errorf("worldline: profile database: %w", err)
	}
	worldAudio := filepath.Join(profileAudio, "worldline")
	manifest, err := scanWorldlineLibrary(library, loc)
	if err != nil {
		return report, err
	}
	db, err := sql.Open("sqlite", paths.Database)
	if err != nil {
		return report, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err = migrations.Apply(ctx, db); err != nil {
		return report, fmt.Errorf("worldline: profile migrations: %w", err)
	}
	var retired int
	if err = db.QueryRowContext(ctx, `SELECT retired FROM profile_control WHERE id=1`).Scan(&retired); err == nil && retired != 0 {
		return report, errors.New("worldline: profile is permanently retired")
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return report, err
	}
	recordings, err := loadProfileSourceRecordings(ctx, db)
	if err != nil {
		return report, err
	}
	if len(recordings) == 0 {
		return report, errors.New("worldline: selected profile has no source-backed recordings")
	}
	prepared := make([]preparedWorldline, 0, len(recordings))
	for _, rec := range recordings {
		item, e := prepareWorldlineRecording(ctx, db, library, originals, worldAudio, loc, rec, manifest)
		if e != nil {
			for _, prior := range prepared {
				if prior.tempPath != "" {
					_ = os.Remove(prior.tempPath)
				}
			}
			return report, e
		}
		prepared = append(prepared, item)
	}
	// Physical outputs are created only after every mapping/hash has validated.
	var created []string
	for i := range prepared {
		if prepared[i].tempPath == "" {
			continue
		}
		if prepared[i].existingPath {
			continue
		}
		if err = installNoReplace(prepared[i].tempPath, prepared[i].finalPath, prepared[i].hash); err != nil {
			_ = os.Remove(prepared[i].tempPath)
			for _, p := range created {
				_ = os.Remove(p)
			}
			return report, err
		}
		created = append(created, prepared[i].finalPath)
		prepared[i].installedNew = true
		prepared[i].entry.AudioPath = filepath.ToSlash(filepath.Join("worldline", filepath.Base(prepared[i].finalPath)))
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		for _, p := range created {
			_ = os.Remove(p)
		}
		return report, err
	}
	rollbackFiles := func() {
		for _, p := range created {
			_ = os.Remove(p)
		}
	}
	for i := range prepared {
		item := &prepared[i]
		if item.fenced || item.skipUpdate {
			continue
		}
		forgotten, fenceErr := forgottenWorldlineRecording(ctx, tx, *item)
		if fenceErr != nil {
			tx.Rollback()
			rollbackFiles()
			return report, fenceErr
		}
		if forgotten {
			item.fenced = true
			item.entry.AudioStatus = "fenced"
			item.entry.AudioPath = ""
			item.entry.Issues = append(item.entry.Issues, "local_forget_tombstone")
			if item.installedNew {
				_ = os.Remove(item.finalPath)
			}
			continue
		}
		if item.setTime && !item.skipUpdate {
			if _, err = tx.ExecContext(ctx, `UPDATE recordings SET recording_started_at=?,absolute_time_known=1 WHERE id=? AND absolute_time_known=0`, item.entry.StartedAt, item.rec.id); err != nil {
				tx.Rollback()
				rollbackFiles()
				return report, err
			}
		}
		if item.providerID == "" || item.skipUpdate {
			continue
		}
		if err = upsertLocalAudio(ctx, tx, *item); err != nil {
			tx.Rollback()
			rollbackFiles()
			return report, err
		}
	}
	if err = tx.Commit(); err != nil {
		rollbackFiles()
		return report, err
	}
	for _, item := range prepared {
		report.Entries = append(report.Entries, item.entry)
		if item.setTime && !item.fenced && !item.skipUpdate {
			report.TimesSet++
		}
		if item.entry.AudioStatus == "ready" || item.entry.AudioStatus == "existing" {
			report.AudioReady++
		} else {
			report.Unavailable++
		}
		report.Issues += len(item.entry.Issues)
	}
	report.Recordings = len(prepared)
	report.Manifest = filepath.Join(paths.Tasks, "recording-time-manifest.json")
	if err = writeWorldlineManifest(report.Manifest, zone, report.Entries); err != nil {
		return report, err
	}
	return report, nil
}

type preparedWorldline struct {
	rec                sourceRecording
	providerID         string
	deviceID           string
	entry              WorldlineRecording
	setTime            bool
	fenced             bool
	skipUpdate         bool
	installedNew       bool
	tempPath           string
	finalPath          string
	hash               string
	existingPath       bool
	totalSamples       int64
	startedAt          string
	transcriptRevision string
}

func loadProfileSourceRecordings(ctx context.Context, db *sql.DB) ([]sourceRecording, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT r.id,r.conversation_id,r.duration_ms,r.recording_started_at,r.absolute_time_known,COALESCE(json_extract(a.source_json,'$.source_audio_sha256'),''),json_extract(a.source_json,'$.recording_id'),json_extract(a.source_json,'$.device_id') FROM recordings r JOIN source_attachments a ON a.recording_id=r.id ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sourceRecording
	seen := map[string]bool{}
	for rows.Next() {
		var r sourceRecording
		if err = rows.Scan(&r.id, &r.conversationID, &r.durationMS, &r.startedAt, &r.timeKnown, &r.sourceAudioHash, &r.providerID, &r.device); err != nil {
			return nil, err
		}
		if r.providerID == "" || r.device == "" {
			continue
		}
		if memory.RecordingID(r.device, r.providerID) != r.id {
			continue
		}
		key := r.id + "\x00" + r.providerID + "\x00" + r.device
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanWorldlineLibrary(root string, loc *time.Location) (map[string][]locatedRun, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	opened, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer opened.Close()
	out := map[string][]locatedRun{}
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("worldline: symlink in library root: %s", e.Name())
		}
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if err = rejectSymlinkDir(dir); err != nil {
			return nil, err
		}
		if err = requireRegularNoSymlink(filepath.Join(dir, "run.json")); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		f, openErr := opened.Open(filepath.Join(e.Name(), "run.json"))
		if errors.Is(openErr, os.ErrNotExist) {
			continue
		}
		if openErr != nil {
			return nil, openErr
		}
		info, statErr := f.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			f.Close()
			return nil, fmt.Errorf("worldline: run.json is not a regular file: %s", e.Name())
		}
		var run runManifest
		dec := json.NewDecoder(io.LimitReader(f, 4<<20))
		decodeErr := dec.Decode(&run)
		f.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("worldline: parse %s/run.json: %w", e.Name(), decodeErr)
		}
		if filepath.Base(run.AudioFileName) != run.AudioFileName {
			return nil, fmt.Errorf("worldline: run %s contains a non-basename audio_file_name", e.Name())
		}
		start, parseErr := ParseWorldlineFilename(run.AudioFileName, loc)
		if parseErr != nil {
			continue
		}
		sum, sumErr := normalizeSHA256(run.AudioSHA256)
		if sumErr != nil {
			return nil, fmt.Errorf("worldline: run %s audio_sha256: %w", e.Name(), sumErr)
		}
		run.AudioSHA256 = sum
		if math.IsNaN(run.Duration) || math.IsInf(run.Duration, 0) || run.Duration <= 0 || run.Duration > 7*24*60*60 {
			return nil, fmt.Errorf("worldline: run %s has invalid duration", e.Name())
		}
		lr := locatedRun{dir: e.Name(), manifest: run, start: start, filename: run.AudioFileName}
		key := start.Format("20060102150405")
		out[key] = append(out[key], lr)
	}
	return out, nil
}

func prepareWorldlineRecording(ctx context.Context, db *sql.DB, library, originals, audioOut string, loc *time.Location, rec sourceRecording, runs map[string][]locatedRun) (preparedWorldline, error) {
	item := preparedWorldline{rec: rec, providerID: rec.providerID, deviceID: rec.device, totalSamples: 0, transcriptRevision: "asr-v1"}
	entry := WorldlineRecording{RecordingID: rec.id, Basis: "filename_capture_timestamp", EndBasis: "media_duration", AudioStatus: "unavailable"}
	idTime, err := ParseRecordingLocator(rec.providerID, loc)
	if err != nil {
		return item, fmt.Errorf("worldline: recording %s has unsupported provider locator", rec.id)
	}
	key := idTime.Format("20060102150405")
	candidates := runs[key]
	var selected *locatedRun
	for i := range candidates {
		if rec.sourceAudioHash != "" && !strings.EqualFold(rec.sourceAudioHash, candidates[i].manifest.AudioSHA256) {
			continue
		}
		if selected != nil {
			return item, fmt.Errorf("worldline: ambiguous library run mapping for recording %s", rec.id)
		}
		selected = &candidates[i]
	}
	if selected == nil {
		return item, fmt.Errorf("worldline: no matching run.json for recording %s", rec.id)
	}
	entry.Filename = selected.filename
	if rec.sourceAudioHash == "" {
		entry.Issues = append(entry.Issues, "run_audio_hash_not_bound_by_source_metadata")
	}
	entry.StartedAt = selected.start.Format(time.RFC3339)
	item.startedAt = entry.StartedAt
	expectedEnd := selected.start.Add(time.Duration(math.Round(selected.manifest.Duration * float64(time.Second))))
	entry.EndAt = expectedEnd.Format(time.RFC3339)
	item.entry = entry
	// A preexisting authoritative start is never silently replaced.
	if rec.timeKnown != 0 && rec.startedAt.Valid {
		known, parseErr := time.Parse(time.RFC3339Nano, rec.startedAt.String)
		if parseErr != nil {
			item.skipUpdate = true
			item.entry.Issues = append(item.entry.Issues, "existing_capture_start_unparseable")
		} else if !known.Equal(selected.start) {
			item.skipUpdate = true
			item.entry.Issues = append(item.entry.Issues, "existing_capture_start_conflict")
		}
	} else {
		item.setTime = true
	}
	if item.skipUpdate {
		item.entry.AudioStatus = "unavailable"
		return item, nil
	}
	if originals != "" {
		root, e := os.OpenRoot(originals)
		if e != nil {
			return item, e
		}
		if err = requireRegularNoSymlink(filepath.Join(originals, selected.filename)); err != nil {
			root.Close()
			return item, err
		}
		f, e := root.Open(selected.filename)
		if e != nil {
			root.Close()
			return item, fmt.Errorf("worldline: original audio missing for %s: %w", selected.filename, e)
		}
		info, e := f.Stat()
		if e == nil && !info.Mode().IsRegular() {
			e = errors.New("original audio is not a regular file")
		}
		if e == nil {
			h := sha256.New()
			_, e = io.Copy(h, f)
			if e == nil && hex.EncodeToString(h.Sum(nil)) != selected.manifest.AudioSHA256 {
				e = fmt.Errorf("original audio hash mismatch for %s", selected.filename)
			}
		}
		f.Close()
		root.Close()
		if e != nil {
			return item, e
		}
		item.entry.OriginalAudioVerified = true
		mtime := info.ModTime().Round(0)
		item.entry.OriginalMtime = mtime.Format(time.RFC3339Nano)
		delta := mtime.Sub(expectedEnd)
		if !mtime.After(selected.start) {
			item.entry.Issues = append(item.entry.Issues, "original_mtime_precedes_capture_start")
			// Keep the source mtime as provenance, but do not expose an invalid
			// playback interval. The declared media duration is the safe bound.
			item.entry.EndBasis = "media_duration_invalid_mtime"
			item.entry.EndAt = expectedEnd.Format(time.RFC3339)
		} else {
			item.entry.EndBasis = "original_mtime"
			item.entry.EndAt = mtime.Format(time.RFC3339Nano)
		}
		if math.Abs(delta.Seconds()) > 600 {
			item.entry.Issues = append(item.entry.Issues, "original_mtime_differs_from_media_duration")
		}
	}
	// Never recreate a recording already fenced by local forgetting.
	forgotten, fenceErr := forgottenWorldlineRecording(ctx, db, item)
	if fenceErr != nil {
		return item, fenceErr
	}
	if forgotten {
		item.fenced = true
		item.entry.AudioStatus = "fenced"
		item.entry.Issues = append(item.entry.Issues, "local_forget_tombstone")
		return item, nil
	}
	var existingDevice, existingPath string
	existingErr := db.QueryRowContext(ctx, `SELECT device_id,COALESCE(stitched_path,'') FROM sync_recordings WHERE recording_id=?`, rec.providerID).Scan(&existingDevice, &existingPath)
	if existingErr == nil {
		if existingDevice != rec.device {
			return item, errors.New("worldline: sync device mismatch")
		}
		if existingPath != "" {
			profileAudioRoot := filepath.Dir(audioOut)
			if filepath.IsAbs(existingPath) && pathWithin(profileAudioRoot, existingPath) && requireRegularNoSymlink(existingPath) == nil {
				item.entry.AudioStatus = "existing"
				rel, _ := filepath.Rel(profileAudioRoot, existingPath)
				item.entry.AudioPath = filepath.ToSlash(rel)
				return item, nil
			}
			item.entry.Issues = append(item.entry.Issues, "existing_audio_mapping_unavailable")
			return item, nil
		}
	} else if !errors.Is(existingErr, sql.ErrNoRows) {
		return item, existingErr
	}
	if err = os.MkdirAll(audioOut, 0700); err != nil {
		return item, err
	}
	if err = rejectSymlinkDir(audioOut); err != nil {
		return item, err
	}
	pcmRoot, err := os.OpenRoot(library)
	if err != nil {
		return item, err
	}
	temp, final, total, hash, stitchErr := stitchWorldlinePCM(pcmRoot, selected, audioOut, library, rec.providerID)
	pcmRoot.Close()
	if stitchErr != nil {
		item.entry.Issues = append(item.entry.Issues, "audio_unavailable:"+stitchErr.Error())
		return item, nil
	}
	item.totalSamples = total / 2
	item.hash = hash
	item.finalPath = final
	if temp == "" {
		item.existingPath = true
		item.entry.AudioStatus = "existing"
	} else {
		item.tempPath = temp
		item.entry.AudioStatus = "ready"
	}
	item.entry.AudioPath = filepath.ToSlash(filepath.Join("worldline", filepath.Base(final)))
	return item, nil
}

// ParseRecordingLocator accepts a provider ID exactly equal to a timestamp or
// recording:<timestamp>, where timestamp is one of the supported filename forms.
func ParseRecordingLocator(id string, loc *time.Location) (time.Time, error) {
	value := id
	if strings.HasPrefix(value, "recording:") {
		value = strings.TrimPrefix(value, "recording:")
	}
	return ParseWorldlineFilename(value+".mp3", loc)
}

func stitchWorldlinePCM(root *os.Root, run *locatedRun, outDir, library, providerID string) (tempPath, finalPath string, totalBytes int64, digest string, err error) {
	segments := append([]runSegment(nil), run.manifest.Segments...)
	if len(segments) == 0 || len(segments) > 20000 {
		return "", "", 0, "", errors.New("run has no valid segments")
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].StartMS < segments[j].StartMS })
	endExpected := int64(math.Round(run.manifest.Duration * 1000))
	tmp, err := os.CreateTemp(outDir, ".worldline-*.tmp")
	if err != nil {
		return "", "", 0, "", err
	}
	tempPath = tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tempPath)
		}
	}()
	if err = tmp.Chmod(0600); err != nil {
		return
	}
	coverage := int64(0)
	for i, seg := range segments {
		if seg.Index <= 0 || seg.StartMS < 0 || seg.EndMS <= seg.StartMS || seg.EndMS > endExpected || filepath.Base(seg.PCM) != seg.PCM || strings.ContainsAny(seg.PCM, `/\\`) {
			err = fmt.Errorf("invalid PCM segment metadata at index %d", i)
			return
		}
		if i == 0 && seg.StartMS != 0 {
			err = errors.New("PCM coverage does not start at zero")
			return
		}
		if seg.StartMS > coverage {
			err = errors.New("PCM segments contain a gap")
			return
		}
		segmentDir := filepath.Join(library, run.dir, "segments")
		if e := rejectSymlinkDir(segmentDir); e != nil {
			err = e
			return
		}
		if e := requireRegularNoSymlink(filepath.Join(segmentDir, seg.PCM)); e != nil {
			err = e
			return
		}
		f, e := root.Open(filepath.Join(run.dir, "segments", seg.PCM))
		if e != nil {
			err = e
			return
		}
		info, e := f.Stat()
		if e != nil || !info.Mode().IsRegular() {
			f.Close()
			if e != nil {
				err = e
			} else {
				err = errors.New("PCM segment is not a regular file")
			}
			return
		}
		want := (seg.EndMS - seg.StartMS) * worldlineBytesPerMS
		if info.Size() != want {
			f.Close()
			err = fmt.Errorf("PCM segment size mismatch at index %d", i)
			return
		}
		overlap := coverage - seg.StartMS
		if overlap > 0 {
			if _, e = tmp.Seek(seg.StartMS*worldlineBytesPerMS, io.SeekStart); e != nil {
				f.Close()
				err = e
				return
			}
			bufA := make([]byte, 32*1024)
			bufB := make([]byte, len(bufA))
			left := overlap * worldlineBytesPerMS
			for left > 0 {
				n := int64(len(bufA))
				if left < n {
					n = left
				}
				if _, e = io.ReadFull(tmp, bufA[:n]); e != nil {
					f.Close()
					err = e
					return
				}
				if _, e = io.ReadFull(f, bufB[:n]); e != nil {
					f.Close()
					err = e
					return
				}
				if !equalPCM(bufA[:n], bufB[:n]) {
					f.Close()
					err = errors.New("overlapping PCM segments disagree")
					return
				}
				left -= n
			}
		}
		if _, e = f.Seek(overlap*worldlineBytesPerMS, io.SeekStart); e != nil {
			f.Close()
			err = e
			return
		}
		if _, e = tmp.Seek(coverage*worldlineBytesPerMS, io.SeekStart); e != nil {
			f.Close()
			err = e
			return
		}
		if _, e = io.Copy(tmp, f); e != nil {
			f.Close()
			err = e
			return
		}
		f.Close()
		coverage = seg.EndMS
	}
	if coverage != endExpected {
		err = errors.New("PCM segments do not cover declared duration")
		return
	}
	if err = tmp.Sync(); err != nil {
		return
	}
	totalBytes = coverage * worldlineBytesPerMS
	if _, err = tmp.Seek(0, io.SeekStart); err != nil {
		return
	}
	h := sha256.New()
	if _, err = io.Copy(h, tmp); err != nil {
		return
	}
	digest = hex.EncodeToString(h.Sum(nil))
	idHash := sha256.Sum256([]byte(providerID))
	finalPath = filepath.Join(outDir, "recording-"+hex.EncodeToString(idHash[:12])+".pcm")
	if err = tmp.Close(); err != nil {
		return
	}
	if existing, openErr := os.Open(finalPath); openErr == nil {
		info, e := existing.Stat()
		if e == nil && info.Mode().IsRegular() && info.Size() == totalBytes {
			hh := sha256.New()
			_, e = io.Copy(hh, existing)
			if e == nil && hex.EncodeToString(hh.Sum(nil)) == digest {
				existing.Close()
				os.Remove(tempPath)
				tempPath = ""
				return
			}
		}
		existing.Close()
		err = errors.New("worldline output path exists with unexpected content")
		return
	} else if !errors.Is(openErr, os.ErrNotExist) {
		err = openErr
		return
	}
	return
}

func equalPCM(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func installNoReplace(temp, target, wantHash string) error {
	if err := os.Link(temp, target); err == nil {
		return os.Remove(temp)
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(target)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("worldline target is not a regular file")
	}
	f, err := os.Open(target)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != wantHash {
		return errors.New("worldline target exists with unexpected content")
	}
	return os.Remove(temp)
}

func upsertLocalAudio(ctx context.Context, tx *sql.Tx, item preparedWorldline) error {
	var existingDevice, existingPath, state string
	err := tx.QueryRowContext(ctx, `SELECT device_id,COALESCE(stitched_path,''),state FROM sync_recordings WHERE recording_id=?`, item.providerID).Scan(&existingDevice, &existingPath, &state)
	if err == nil && existingDevice != item.deviceID {
		return fmt.Errorf("worldline: sync device mismatch for recording %s", item.providerID)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	path := existingPath
	if item.entry.AudioStatus == "ready" {
		if path != "" && path != item.finalPath {
			return errors.New("worldline: refusing to replace existing stitched audio mapping")
		}
		path = item.finalPath
	}
	if state == "deleted" {
		return errors.New("worldline: refusing to restore deleted audio")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sync_recordings(recording_id,owner,device_id,state,total_samples,started_at,stitched_path,transcript_revision,transcription_state,updated_at) VALUES(?,?,?,'downloaded',?,?,?,'asr-v1','none',?) ON CONFLICT(recording_id) DO UPDATE SET total_samples=CASE WHEN sync_recordings.total_samples IS NULL OR sync_recordings.total_samples<=0 THEN excluded.total_samples ELSE sync_recordings.total_samples END,started_at=COALESCE(sync_recordings.started_at,excluded.started_at),stitched_path=CASE WHEN sync_recordings.stitched_path IS NULL OR sync_recordings.stitched_path='' THEN excluded.stitched_path ELSE sync_recordings.stitched_path END,updated_at=excluded.updated_at`, item.providerID, "local-worldline", item.deviceID, item.totalSamples, item.startedAt, nullIfEmpty(path), time.Now().UnixMilli())
	return err
}

func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func forgottenWorldlineRecording(ctx context.Context, q rowQuerier, item preparedWorldline) (bool, error) {
	var blocked int
	err := q.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM forget_provider_fences WHERE provider_recording_id=? AND device_id=?) OR
		EXISTS(SELECT 1 FROM forget_tombstones WHERE recording_id=?) OR
		EXISTS(SELECT 1 FROM forget_tombstones WHERE scope='conversation' AND conversation_id=?) OR
		EXISTS(SELECT 1 FROM sync_recordings WHERE recording_id=? AND device_id=? AND (state='deleted' OR transcription_state='forgotten')) OR
		EXISTS(SELECT 1 FROM profile_control WHERE id=1 AND retired=1)`,
		item.providerID, item.deviceID, item.rec.id, item.rec.conversationID, item.providerID, item.deviceID).Scan(&blocked)
	return blocked != 0, err
}

func writeWorldlineManifest(path, zone string, entries []WorldlineRecording) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	m := worldlineManifest{Version: 1, PreparedAt: time.Now().UTC().Format(time.RFC3339Nano), Timezone: zone, Recordings: entries}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.CreateTemp(filepath.Dir(path), ".recording-time-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if info, e := os.Lstat(path); e == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return errors.New("worldline manifest target is not a regular file")
	}
	return os.Rename(tmp, path)
}

func normalizeSHA256(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	b, err := hex.DecodeString(v)
	if err != nil || len(b) != sha256.Size {
		return "", errors.New("expected 64 hex characters")
	}
	return v, nil
}

func rejectSymlinkDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("worldline: expected non-symlink directory: %s", path)
	}
	return nil
}

func requireRegularNoSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("worldline: expected regular non-symlink file: %s", path)
	}
	return nil
}

func pathWithin(root, path string) bool {
	r, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	p, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(r, p)
	return err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func rejectSymlinkComponents(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(abs)
	rest := strings.TrimPrefix(abs, volume)
	current := volume + string(os.PathSeparator)
	for _, part := range strings.Split(strings.TrimLeft(rest, string(os.PathSeparator)), string(os.PathSeparator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, e := os.Lstat(current)
		if e != nil {
			return e
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("worldline: symlink path component: %s", current)
		}
	}
	return nil
}
