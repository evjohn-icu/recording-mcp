package cloudsync

import (
	"context"
	"database/sql"
	"errors"
	"os"
)

var errLocallyForgotten = errors.New("recording has a local forgetting fence")

// Provider IDs survive cursor resets and source revision replacement. A
// granular local forget also fences provider replay: downloading the original
// recording again would restore the erased audio interval.
func (e *Engine) locallyForgotten(ctx context.Context, providerID string) (bool, error) {
	var found bool
	err := e.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM profile_control WHERE id=1 AND retired=1) OR EXISTS(SELECT 1 FROM forget_provider_fences WHERE provider_recording_id=?)`, providerID).Scan(&found)
	return found, err
}

func forgottenTx(ctx context.Context, tx *sql.Tx, providerID string) (bool, error) {
	var found bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM profile_control WHERE id=1 AND retired=1) OR EXISTS(SELECT 1 FROM forget_provider_fences WHERE provider_recording_id=?)`, providerID).Scan(&found)
	return found, err
}

func fullForgetTx(ctx context.Context, tx *sql.Tx, providerID string) (bool, error) {
	var full bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM profile_control WHERE id=1 AND retired=1) OR EXISTS(SELECT 1 FROM forget_provider_fences f JOIN forget_tombstones t ON t.id=f.tombstone_id WHERE f.provider_recording_id=? AND t.scope IN ('recording','conversation'))`, providerID).Scan(&full)
	return full, err
}

func clearForgottenRecordingTx(ctx context.Context, tx *sql.Tx, providerID string, now int64, full bool) error {
	if err := upsertRecordingTx(ctx, tx, providerID, "", "", now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sync_events SET payload_json='{}' WHERE recording_id=?`, providerID); err != nil {
		return err
	}
	if full {
		if err := deleteChunksTx(ctx, tx, providerID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE sync_recordings SET state='deleted',transcription_state='forgotten',stitched_path=NULL,last_error='forgotten',updated_at=? WHERE recording_id=?`, now, providerID)
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE sync_recordings SET transcription_state='forgotten',last_error='forgotten',updated_at=? WHERE recording_id=?`, now, providerID)
	return err
}

func (e *Engine) requireNotForgotten(ctx context.Context, providerID string) error {
	found, err := e.locallyForgotten(ctx, providerID)
	if err != nil {
		return err
	}
	if found {
		return errLocallyForgotten
	}
	return nil
}

// finalizeDownload serializes the final rename with the durable forget fence.
// A network request started before forgetting may finish, but cannot publish
// its bytes after the fence commits.
func (e *Engine) finalizeDownload(ctx context.Context, providerID, part, final string) error {
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE sync_state SET updated_at=updated_at WHERE id=1`); err != nil {
		return err
	}
	var blocked bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM forget_provider_fences WHERE provider_recording_id=?)`, providerID).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return errLocallyForgotten
	}
	if err = os.Rename(part, final); err != nil {
		return err
	}
	return tx.Commit()
}
