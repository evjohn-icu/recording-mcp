package ingest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"os"

	"recordingmcp/internal/audioanalysis"
	"recordingmcp/internal/process"
)

// audioCapabilities are enqueued automatically by the ingest stage, in order.
// Speaker mapping is explicit only: it needs the attachments the ASR stage has
// not written yet at ingest time.
var audioCapabilities = []string{audioanalysis.CapabilityVAD, audioanalysis.CapabilityTagging, audioanalysis.CapabilityDiarization}

// audioPlan is one pre-validated audio request for the ingest transaction.
type audioPlan struct {
	capability string
	request    audioanalysis.Request
	generation string
}

// audioPlans builds the enabled, verified audio requests for one import. A
// capability whose model is unavailable is skipped; it must not block ASR.
func (s *Service) audioPlans(ctx context.Context, recordingID, sourceSHA, pcmAbs string, durationMS int64) ([]audioPlan, error) {
	if s.audioCfg == nil || s.audio == nil || durationMS <= 0 {
		return nil, nil
	}
	inputHash, err := hashFileBytes(ctx, pcmAbs)
	if err != nil {
		return nil, err
	}
	source := sourceSHA
	if len(source) != 64 {
		source = inputHash
	}
	plans := make([]audioPlan, 0, len(audioCapabilities))
	for _, capability := range audioCapabilities {
		resolved, err := s.audioCfg.Resolve(capability)
		if err != nil {
			continue
		}
		generation := audioGeneration(source, capability, resolved.ModelRevision, resolved.ConfigSHA256)
		files := make(map[string]string, len(resolved.Files))
		for _, file := range resolved.Files {
			files[file.Path] = file.SHA256
		}
		req := audioanalysis.Request{
			SchemaVersion:       audioanalysis.ContractSchema,
			RecordingID:         recordingID,
			SourceAudioSHA256:   source,
			InputSHA256:         inputHash,
			ModelID:             resolved.ModelID,
			ModelRevision:       resolved.ModelRevision,
			ModelFilesSHA256:    files,
			Capability:          capability,
			StartMS:             0,
			EndMS:               durationMS,
			ConfigSHA256:        resolved.ConfigSHA256,
			SourceAttachmentIDs: []string{},
		}
		req.RunID = audioanalysis.ComputeRunID(req, generation)
		plans = append(plans, audioPlan{capability: capability, request: req, generation: generation})
	}
	return plans, nil
}

// audioGeneration pins source, capability, model revision and config so a model
// upgrade or reconfiguration always needs a new generation.
func audioGeneration(source, capability, revision, configHash string) string {
	sum := sha256.Sum256([]byte(source + "\x00" + capability + "\x00" + revision + "\x00" + configHash))
	return "ingest-" + hex.EncodeToString(sum[:])[:32]
}

// hashFileBytes hashes one managed file.
func hashFileBytes(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	buf := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := file.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// enqueueAudioTx enqueues one planned audio run with its dependencies.
func (s *Service) enqueueAudioTx(ctx context.Context, tx *sql.Tx, recordingID string, plan audioPlan, dependencies []string) (string, error) {
	out, err := s.audio.EnqueueTx(ctx, tx, audioanalysis.EnqueueInput{
		RecordingID:  recordingID,
		Capability:   plan.capability,
		Generation:   plan.generation,
		Request:      plan.request,
		Dependencies: dependencies,
		MaxAttempts:  3,
	})
	if err != nil {
		return "", err
	}
	return out.JobID, nil
}

// nonEmptyDependencies drops empty ids so an absent parent adds no edge.
func nonEmptyDependencies(ids ...string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

var _ = process.Spec{}
