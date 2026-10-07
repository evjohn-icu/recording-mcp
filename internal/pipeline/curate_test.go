package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/memory"
	"recordingmcp/internal/migrations"
	"recordingmcp/internal/process"
)

// curateJobs returns the idempotency key and payload of every curate job for a
// conversation, oldest first.
func curateJobs(t *testing.T, db *sql.DB, conversationID string) [][2]string {
	t.Helper()
	rows, err := db.Query(`SELECT idempotency_key, payload_json FROM processing_jobs WHERE stage = 'curate' ORDER BY created_at, id`)
	if err != nil {
		t.Fatalf("query curate jobs: %v", err)
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var key, payload string
		if err := rows.Scan(&key, &payload); err != nil {
			t.Fatalf("scan curate job: %v", err)
		}
		if strings.Contains(key, conversationID) {
			out = append(out, [2]string{key, payload})
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read curate jobs: %v", err)
	}
	return out
}

// seedConversationVersion inserts a bare conversations row carrying the given
// annotation version, mirroring the cmd/context-memory seed style. The curate
// guard only reads the version column, so every other column is a placeholder.
func seedConversationVersion(t *testing.T, db *sql.DB, conversationID, version string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json)
		VALUES(?,'title','',?,'','rec:seed','sha','v1',1000,'{}','{}')`, conversationID, version); err != nil {
		t.Fatalf("seed conversation %s: %v", conversationID, err)
	}
}

// claimCurateJob enqueues and claims one curate job. The key is separate from
// the payload version so a test can run the same curation twice, which is what
// a retried or re-triggered job looks like.
func claimCurateJob(t *testing.T, db *sql.DB, conversationID, version, key string) process.Job {
	t.Helper()
	spec := process.Spec{
		Stage:           "curate",
		InputSnapshotID: conversationID,
		IdempotencyKey:  "curate|" + conversationID + "|" + key,
		PayloadJSON:     `{"conversation_id":"` + conversationID + `","annotation_version":"` + version + `"}`,
		MaxAttempts:     3,
	}
	if _, _, err := process.Enqueue(context.Background(), db, spec, time.Now()); err != nil {
		t.Fatalf("enqueue curate: %v", err)
	}
	job, err := process.Claim(context.Background(), db, process.ClaimOptions{
		WorkerID: "curate-worker", Stages: []string{"curate"}, Lease: time.Minute, Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("claim curate: %v", err)
	}
	return *job
}

// publishOne runs one publish for a conversation with two validated candidates
// and returns the published annotation version.
func publishOne(t *testing.T, db *sql.DB, store *memory.Store, conversationID, runID string) string {
	t.Helper()
	ref, err := (&PublishHandler{DB: db, Store: store, ProcessorVersion: "publish-v1"}).
		Execute(context.Background(), claimPublishJob(t, db, conversationID, runID))
	if err != nil {
		t.Fatalf("publish %s: %v", conversationID, err)
	}
	if !strings.HasPrefix(ref, "publish:") || ref == "publish:noop" {
		t.Fatalf("publish ref = %q, want a committed version", ref)
	}
	return strings.TrimPrefix(ref, "publish:")
}

// proposalJSON renders one topic-link proposal for the topics and turns a
// published conversation produced.
func proposalJSON(t *testing.T, db *sql.DB, conversationID, relation, reason string) string {
	t.Helper()
	var fromID, toID string
	rows, err := db.Query(`SELECT id FROM topics WHERE id LIKE ? ORDER BY id`, autoTopicPrefix+"%")
	if err != nil {
		t.Fatalf("query topics: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan topic: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read topics: %v", err)
	}
	if len(ids) < 2 {
		t.Fatalf("conversation %s published %d auto topics, want at least 2", conversationID, len(ids))
	}
	fromID, toID = ids[0], ids[1]
	return fmt.Sprintf(`{"proposals":[{"kind":"topic_link","from_topic_id":%q,"to_topic_id":%q,"relation":%q,"reason":%q,"evidence_turn_ids":["t1"],"confidence":0.8}]}`,
		fromID, toID, relation, reason)
}

// TestPublishEnqueuesCurate checks that only a committed publish schedules
// curation, with a key pairing the conversation and the published version.
func TestPublishEnqueuesCurate(t *testing.T) {
	db, store := openTestDBAndStore(t)
	ctx := context.Background()
	seedTwoTurns(t, db, "conv:1")
	insertValidated(t, db, "conv:1", "run-1")

	ref, err := (&PublishHandler{DB: db, Store: store, ProcessorVersion: "publish-v1"}).
		Execute(ctx, claimPublishJob(t, db, "conv:1", "run-1"))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	version := strings.TrimPrefix(ref, "publish:")
	jobs := curateJobs(t, db, "conv:1")
	if len(jobs) != 1 {
		t.Fatalf("curate jobs = %d, want 1: %+v", len(jobs), jobs)
	}
	if want := "curate|conv:1|" + version; jobs[0][0] != want {
		t.Errorf("idempotency key = %q, want %q", jobs[0][0], want)
	}
	if !strings.Contains(jobs[0][1], `"annotation_version":"`+version+`"`) {
		t.Errorf("payload = %q, want the published version", jobs[0][1])
	}

	// Replaying the same publish must not queue a second curation, and a
	// conversation without validated candidates publishes nothing at all.
	if _, err := (&PublishHandler{DB: db, Store: store, ProcessorVersion: "publish-v1"}).
		Execute(ctx, claimPublishJob(t, db, "conv:1", "run-1-replay")); err != nil {
		t.Fatalf("replay publish: %v", err)
	}
	if jobs := curateJobs(t, db, "conv:1"); len(jobs) != 1 {
		t.Errorf("replay queued %d curate jobs, want 1: %+v", len(jobs), jobs)
	}
	// A conversation without validated candidates publishes nothing at all.
	noop, err := (&PublishHandler{DB: db, Store: store, ProcessorVersion: "publish-v1"}).
		Execute(ctx, claimPublishJob(t, db, "conv:2", "run-2"))
	if err != nil || noop != "publish:noop" {
		t.Fatalf("empty publish = %q, %v; want publish:noop", noop, err)
	}
	if jobs := curateJobs(t, db, "conv:2"); len(jobs) != 0 {
		t.Errorf("noop publish queued %+v", jobs)
	}
}

// TestCurateHandlerStoresProposals runs the whole curate path: an external
// command writes a proposal file, the handler validates it against the real
// topics and turns, and the link lands in memory_links attributed to the
// configured model.
func TestCurateHandlerStoresProposals(t *testing.T) {
	db, store := openTestDBAndStore(t)
	ctx := context.Background()
	seedTwoTurns(t, db, "conv:1")
	insertValidated(t, db, "conv:1", "run-1")
	version := publishOne(t, db, store, "conv:1", "run-1")

	proposal := proposalJSON(t, db, "conv:1", "same_topic", "同一段讨论的两个话题")
	handler := &CurateHandler{
		DB:      db,
		DBPath:  "/tmp/unused.db",
		Command: "printf '%s' '" + proposal + "' > \"$CONTEXT_PROPOSALS\"",
		Model:   "stub-curator",
	}
	ref, err := handler.Execute(ctx, claimCurateJob(t, db, "conv:1", version, "auto-v1"))
	if err != nil {
		t.Fatalf("curate: %v", err)
	}
	if want := "curate:" + version + "+proposals:1+rejected:0+written:1"; ref != want {
		t.Fatalf("ref = %q, want %q", ref, want)
	}
	links, err := ListLinks(ctx, db, LinkFilter{})
	if err != nil || len(links) != 1 {
		t.Fatalf("links = %+v, %v", links, err)
	}
	link := links[0]
	if link.Status != LinkStatusProposed || link.Relation != "same_topic" || link.Model != "stub-curator" {
		t.Errorf("link = %+v, want a proposed same_topic link from stub-curator", link)
	}
	if len(link.EvidenceTurnIDs) != 1 || link.EvidenceTurnIDs[0] != "t1" {
		t.Errorf("evidence = %v, want [t1]", link.EvidenceTurnIDs)
	}
	if link.FromTitle == "" || link.ToTitle == "" {
		t.Errorf("titles not resolved: %+v", link)
	}

	// Running the same curation again is idempotent.
	if ref, err := handler.Execute(ctx, claimCurateJob(t, db, "conv:1", version, "auto-v1")); err != nil {
		t.Fatalf("replay curate: %v", err)
	} else if !strings.Contains(ref, "written:0") {
		t.Errorf("replay ref = %q, want no writes", ref)
	}
	if links, err := ListLinks(ctx, db, LinkFilter{}); err != nil || len(links) != 1 {
		t.Fatalf("links after replay = %+v, %v", links, err)
	}

	// A proposal citing an unknown turn is rejected by validation, and the
	// handler reports it instead of writing it.
	bad := strings.Replace(proposal, `"t1"`, `"turn:missing"`, 1)
	badHandler := &CurateHandler{DB: db, DBPath: "/tmp/unused.db", Command: "printf '%s' '" + bad + "' > \"$CONTEXT_PROPOSALS\"", Model: "stub-curator"}
	ref, err = badHandler.Execute(ctx, claimCurateJob(t, db, "conv:1", version, "auto-v1-bad-evidence"))
	if err != nil {
		t.Fatalf("curate with bad evidence: %v", err)
	}
	if !strings.Contains(ref, "+rejected:1+written:0") {
		t.Errorf("ref = %q, want the rejected proposal counted", ref)
	}
	if links, err := ListLinks(ctx, db, LinkFilter{}); err != nil || len(links) != 1 {
		t.Fatalf("links after rejected proposal = %+v, %v", links, err)
	}
}

// TestCurateHandlerExportsCommandContract checks the environment a curator
// command can rely on: the database path, the proposals path and the job's
// conversation and version.
func TestCurateHandlerExportsCommandContract(t *testing.T) {
	db, _ := openTestDBAndStore(t)
	seedConversationVersion(t, db, "conv:7", "auto-abc123")
	side := filepath.Join(t.TempDir(), "env.txt")
	handler := &CurateHandler{
		DB:     db,
		DBPath: "/data/context-memory.db",
		Command: `printf '%s\n' "$CONTEXT_DB" "$CONTEXT_CURATE_CONVERSATION" "$CONTEXT_CURATE_VERSION" > ` + side +
			` && printf '%s' '{"proposals":[]}' > "$CONTEXT_PROPOSALS"`,
		Model: "stub-curator",
	}
	ref, err := handler.Execute(context.Background(), claimCurateJob(t, db, "conv:7", "auto-abc123", "auto-abc123"))
	if err != nil {
		t.Fatalf("curate: %v", err)
	}
	if ref != "curate:auto-abc123+proposals:0+rejected:0+written:0" {
		t.Errorf("ref = %q", ref)
	}
	got, err := os.ReadFile(side)
	if err != nil {
		t.Fatalf("read side file: %v", err)
	}
	want := "/data/context-memory.db\nconv:7\nauto-abc123\n"
	if string(got) != want {
		t.Errorf("env = %q, want %q", got, want)
	}
}

// TestCurateHandlerFailureModes covers the command contract violations a job
// must surface: a failing command, a missing proposal file and malformed JSON.
func TestCurateHandlerFailureModes(t *testing.T) {
	db, _ := openTestDBAndStore(t)
	ctx := context.Background()
	seedConversationVersion(t, db, "conv:1", "auto-v1")

	cases := []struct {
		name    string
		handler *CurateHandler
		want    string
	}{
		{
			name:    "command fails",
			handler: &CurateHandler{DB: db, DBPath: "/tmp/x.db", Command: "echo blew up >&2; exit 3"},
			want:    "curator command failed",
		},
		{
			name:    "writes nothing",
			handler: &CurateHandler{DB: db, DBPath: "/tmp/x.db", Command: "true"},
			want:    "read curator proposals",
		},
		{
			name:    "writes malformed json",
			handler: &CurateHandler{DB: db, DBPath: "/tmp/x.db", Command: `printf '%s' 'not json' > "$CONTEXT_PROPOSALS"`},
			want:    "read curator proposals",
		},
		{
			name:    "times out",
			handler: &CurateHandler{DB: db, DBPath: "/tmp/x.db", Command: "sleep 5", Timeout: 150 * time.Millisecond},
			want:    "timed out",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := tc.handler.Execute(ctx, claimCurateJob(t, db, "conv:1", "auto-v1", tc.name))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v (ref %q), want it to mention %q", err, ref, tc.want)
			}
		})
	}
}

// TestCurateHandlerSkipsWithoutCommand keeps an unconfigured worker honest: the
// job completes as skipped, and nothing is written.
func TestCurateHandlerSkipsWithoutCommand(t *testing.T) {
	db, _ := openTestDBAndStore(t)
	handler := &CurateHandler{DB: db, DBPath: "/tmp/x.db"}
	ref, err := handler.Execute(context.Background(), claimCurateJob(t, db, "conv:1", "auto-v1", "auto-v1"))
	if err != nil {
		t.Fatalf("curate: %v", err)
	}
	if ref != "curate:auto-v1+skipped:no-command" {
		t.Errorf("ref = %q, want a skipped result", ref)
	}
	links, err := ListLinks(context.Background(), db, LinkFilter{})
	if err != nil || len(links) != 0 {
		t.Fatalf("skipped curation wrote links: %+v, %v", links, err)
	}
}

// TestCurateHandlerSkipsSupersededVersion keeps stale work from curating a
// version that no longer holds: the job completes as skipped, the curator
// never runs, and the proposal it would have written is not stored.
func TestCurateHandlerSkipsSupersededVersion(t *testing.T) {
	db, _ := openTestDBAndStore(t)
	ctx := context.Background()
	seedTwoTurns(t, db, "conv:1")
	if _, err := db.Exec(`UPDATE conversations SET annotation_version = 'auto-v2' WHERE id = 'conv:1'`); err != nil {
		t.Fatalf("set current version: %v", err)
	}
	// Seed topics and a turn the validator accepts, so the empty-links
	// assertion below is load-bearing only if the guard holds.
	seedTopic(t, db, "topic-1")
	seedTopic(t, db, "topic-2")
	proposal := `{"proposals":[{"kind":"topic_link","from_topic_id":"topic-1","to_topic_id":"topic-2","relation":"same_topic","reason":"would be accepted","evidence_turn_ids":["t1"],"confidence":0.8}]}`
	side := filepath.Join(t.TempDir(), "ran.txt")
	handler := &CurateHandler{
		DB:      db,
		DBPath:  "/tmp/unused.db",
		Command: `touch "` + side + `" && printf '%s' '` + proposal + `' > "$CONTEXT_PROPOSALS"`,
		Model:   "stub-curator",
	}
	ref, err := handler.Execute(ctx, claimCurateJob(t, db, "conv:1", "auto-v1", "auto-v1"))
	if err != nil {
		t.Fatalf("curate: %v", err)
	}
	if ref != "curate:auto-v1+skipped:superseded" {
		t.Fatalf("ref = %q, want curate:auto-v1+skipped:superseded", ref)
	}
	if _, err := os.Stat(side); !os.IsNotExist(err) {
		t.Errorf("stale job ran the curator: stat err = %v, want not-exist", err)
	}
	links, err := ListLinks(ctx, db, LinkFilter{})
	if err != nil || len(links) != 0 {
		t.Fatalf("stale curation wrote links: %+v, %v", links, err)
	}
}

// TestCurateHandlerErrorsOnMissingConversation refuses to curate a
// conversation the database does not know: the missing row is an error, never
// a silent skip that hides a broken payload.
func TestCurateHandlerErrorsOnMissingConversation(t *testing.T) {
	db, _ := openTestDBAndStore(t)
	handler := &CurateHandler{
		DB:      db,
		DBPath:  "/tmp/unused.db",
		Command: `printf '%s' '{"proposals":[]}' > "$CONTEXT_PROPOSALS"`,
	}
	_, err := handler.Execute(context.Background(), claimCurateJob(t, db, "conv:missing", "auto-v1", "auto-v1"))
	if err == nil || !strings.Contains(err.Error(), "conversation not found") {
		t.Fatalf("err = %v, want it to mention the missing conversation", err)
	}
}

// TestCurateHandlerRejectsMissingVersion treats an empty annotation_version as
// a malformed payload: publish always enqueues auto-<hash>, so an empty
// version can only come from a hand-built job.
func TestCurateHandlerRejectsMissingVersion(t *testing.T) {
	db, _ := openTestDBAndStore(t)
	ctx := context.Background()
	handler := &CurateHandler{DB: db, DBPath: "/tmp/unused.db", Command: "true"}
	_, err := handler.Execute(ctx, claimCurateJob(t, db, "conv:1", "", "empty"))
	if err == nil || !strings.Contains(err.Error(), "annotation_version is required") {
		t.Fatalf("err = %v, want it to mention annotation_version", err)
	}
	if _, err := handler.Execute(ctx, claimCurateJob(t, db, "conv:1", "   ", "blank")); err == nil ||
		!strings.Contains(err.Error(), "annotation_version is required") {
		t.Fatalf("whitespace-only version err = %v, want it to mention annotation_version", err)
	}
	links, err := ListLinks(ctx, db, LinkFilter{})
	if err != nil || len(links) != 0 {
		t.Fatalf("malformed job wrote links: %+v, %v", links, err)
	}
}

// TestCurateHandlerRejectsPaddedVersion treats a padded annotation_version as
// a malformed payload: it errors loudly instead of running against the trimmed
// value or being mistaken for a superseded job.
func TestCurateHandlerRejectsPaddedVersion(t *testing.T) {
	db, _ := openTestDBAndStore(t)
	ctx := context.Background()
	seedConversationVersion(t, db, "conv:1", "auto-v1")
	side := filepath.Join(t.TempDir(), "ran.txt")
	handler := &CurateHandler{
		DB:      db,
		DBPath:  "/tmp/unused.db",
		Command: `touch "` + side + `" && printf '%s' '{"proposals":[]}' > "$CONTEXT_PROPOSALS"`,
	}
	_, err := handler.Execute(ctx, claimCurateJob(t, db, "conv:1", "  auto-v1  ", "padded"))
	if err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Fatalf("err = %v, want a malformed-version error", err)
	}
	if _, statErr := os.Stat(side); !os.IsNotExist(statErr) {
		t.Errorf("padded-version job ran the curator: stat err = %v, want not-exist", statErr)
	}
	if links, err := ListLinks(ctx, db, LinkFilter{}); err != nil || len(links) != 0 {
		t.Fatalf("padded-version job wrote links: %+v, %v", links, err)
	}
}

// TestCurateStaleJobNeverWritesLinks is the end-to-end staleness invariant:
// publish v1 enqueues a curate job, publish v2 supersedes it, and running the
// v1 job afterwards writes nothing while the v2 job still curates normally.
func TestCurateStaleJobNeverWritesLinks(t *testing.T) {
	db, store := openTestDBAndStore(t)
	ctx := context.Background()
	seedTwoTurns(t, db, "conv:1")
	insertValidated(t, db, "conv:1", "run-1")
	v1 := publishOne(t, db, store, "conv:1", "run-1")

	// A changed candidate set publishes its own version and supersedes the
	// v1 curate job that is still queued.
	if _, err := InsertCandidates(ctx, db, "conv:1", "run-2", "job-run-2", "synthetic", []ValidatedDraft{
		{
			CandidateDraft: CandidateDraft{
				Kind: "observation", Summary: "排期已确认", TopicTitle: "排期",
				EvidenceTurnIDs: []string{"t1"}, InterpretationNote: "note-run-2",
			},
			Status: "validated",
		},
	}, time.Now()); err != nil {
		t.Fatalf("insert candidates run-2: %v", err)
	}
	v2 := publishOne(t, db, store, "conv:1", "run-2")
	if v1 == v2 {
		t.Fatalf("both publishes produced version %s, want distinct versions", v1)
	}

	jobs := curateJobs(t, db, "conv:1")
	if len(jobs) != 2 {
		t.Fatalf("curate jobs = %d, want 2 (one per publish): %+v", len(jobs), jobs)
	}
	// curateJobs orders by (created_at, id); pick each job by the version its
	// payload carries so the test never depends on that tiebreak.
	payloadFor := func(version string) string {
		t.Helper()
		marker := `"annotation_version":"` + version + `"`
		for _, job := range jobs {
			if strings.Contains(job[1], marker) {
				return job[1]
			}
		}
		t.Fatalf("no curate job payload for version %s: %+v", version, jobs)
		return ""
	}
	v1Payload, v2Payload := payloadFor(v1), payloadFor(v2)

	proposal := proposalJSON(t, db, "conv:1", "same_topic", "同一段讨论的两个话题")
	side := filepath.Join(t.TempDir(), "ran.txt")
	handler := &CurateHandler{
		DB:      db,
		DBPath:  "/tmp/unused.db",
		Command: `touch "` + side + `" && printf '%s' '` + proposal + `' > "$CONTEXT_PROPOSALS"`,
		Model:   "stub-curator",
	}

	// The v1 job runs after v2 was published: stale work, no curator, no writes.
	ref, err := handler.Execute(ctx, process.Job{PayloadJSON: v1Payload})
	if err != nil {
		t.Fatalf("stale curate: %v", err)
	}
	if want := "curate:" + v1 + "+skipped:superseded"; ref != want {
		t.Fatalf("stale ref = %q, want %q", ref, want)
	}
	if _, err := os.Stat(side); !os.IsNotExist(err) {
		t.Errorf("stale job ran the curator: stat err = %v, want not-exist", err)
	}
	if links, err := ListLinks(ctx, db, LinkFilter{}); err != nil || len(links) != 0 {
		t.Fatalf("stale curation wrote links: %+v, %v", links, err)
	}

	// The v2 job is current work and must still write the link, which proves
	// the guard is not over-broad.
	ref, err = handler.Execute(ctx, process.Job{PayloadJSON: v2Payload})
	if err != nil {
		t.Fatalf("current curate: %v", err)
	}
	if want := "curate:" + v2 + "+proposals:1+rejected:0+written:1"; ref != want {
		t.Fatalf("current ref = %q, want %q", ref, want)
	}
	if _, err := os.Stat(side); err != nil {
		t.Errorf("current job did not run the curator: %v", err)
	}
	if links, err := ListLinks(ctx, db, LinkFilter{}); err != nil || len(links) != 1 {
		t.Fatalf("current curation links = %+v, %v; want 1", links, err)
	}
}

// TestCurateHelperBumpVersion is a subprocess target, not a regular test: the
// freshness-recheck regression re-execs the test binary as its curator command
// so the stored annotation version changes between Execute's two freshness
// checks without external tools.
func TestCurateHelperBumpVersion(t *testing.T) {
	if os.Getenv("GO_WANT_CURATE_HELPER") != "1" {
		t.Skip("curate helper subprocess")
	}
	db, err := sql.Open("sqlite", os.Getenv("CONTEXT_DB"))
	if err != nil {
		t.Fatalf("helper: open db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE conversations SET annotation_version = ? WHERE id = ?`,
		os.Getenv("BUMP_VERSION"), os.Getenv("BUMP_CONVERSATION")); err != nil {
		t.Fatalf("helper: bump version: %v", err)
	}
}

// TestCurateHandlerRechecksVersionBeforeWriting pins the second freshness
// check: a version change while the curator runs leaves no links behind,
// exactly like a job superseded before it started.
func TestCurateHandlerRechecksVersionBeforeWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recheck.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	seedConversationVersion(t, db, "conv:1", "auto-v1")
	seedTopic(t, db, "topic-1")
	seedTopic(t, db, "topic-2")
	seedTurn(t, db, "t1")

	t.Setenv("GO_WANT_CURATE_HELPER", "1")
	t.Setenv("BUMP_VERSION", "auto-v2")
	t.Setenv("BUMP_CONVERSATION", "conv:1")
	proposal := `{"proposals":[{"kind":"topic_link","from_topic_id":"topic-1","to_topic_id":"topic-2","relation":"same_topic","reason":"superseded mid-run","evidence_turn_ids":["t1"],"confidence":0.8}]}`
	side := filepath.Join(t.TempDir(), "ran.txt")
	handler := &CurateHandler{
		DB:     db,
		DBPath: path,
		Command: fmt.Sprintf("%q -test.run=TestCurateHelperBumpVersion && printf '%%s' '%s' > \"$CONTEXT_PROPOSALS\" && touch %q",
			os.Args[0], proposal, side),
		Model: "stub-curator",
	}
	ctx := context.Background()
	ref, err := handler.Execute(ctx, claimCurateJob(t, db, "conv:1", "auto-v1", "recheck"))
	if err != nil {
		t.Fatalf("curate: %v", err)
	}
	if ref != "curate:auto-v1+skipped:superseded" {
		t.Fatalf("ref = %q, want curate:auto-v1+skipped:superseded", ref)
	}
	if _, err := os.Stat(side); err != nil {
		t.Fatalf("curator did not run: %v", err)
	}
	if links, err := ListLinks(ctx, db, LinkFilter{}); err != nil || len(links) != 0 {
		t.Fatalf("superseded mid-run job wrote links: %+v, %v", links, err)
	}
}
