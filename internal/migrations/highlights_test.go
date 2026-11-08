package migrations

import (
	"context"
	"testing"
)

func TestHighlightsUpgradeFromV18InstallsRetirementFence(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if e := Apply(ctx, db); e != nil {
		t.Fatal(e)
	}
	for _, q := range []string{`DROP TABLE memory_highlight_requests`, `DROP TABLE memory_highlights`, `DELETE FROM schema_migrations WHERE version>=19`, `INSERT INTO profile_control(id,retired,tombstone_id,created_at) VALUES(1,1,'retired-before-upgrade',1)`} {
		if _, e := db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	for i := 0; i < 2; i++ {
		if e := Apply(ctx, db); e != nil {
			t.Fatal(e)
		}
	}
	if v := mustVersion(t, ctx, db); v != len(migrations) {
		t.Fatalf("version %d, want latest %d", v, len(migrations))
	}
	for _, q := range []string{`INSERT INTO memory_highlights(id,conversation_id,recording_id,anchor_kind,tag,created_at_ms,updated_at_ms) VALUES('h','c','r','turn','important',1,1)`, `INSERT INTO memory_highlight_requests(request_id,conversation_id,recording_id,anchor_kind,payload_sha256,highlight_id,status,created_at_ms) VALUES('q','c','r','turn','hash','h','active',1)`} {
		if _, e := db.Exec(q); e == nil {
			t.Fatal("retired insert allowed")
		}
	}
	for _, table := range []string{"memory_highlights", "memory_highlight_requests"} {
		if _, e := db.Exec("DELETE FROM " + table); e != nil {
			t.Fatalf("purge blocked: %v", e)
		}
	}
}
