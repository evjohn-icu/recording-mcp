package ingest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"

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

// audioPlans builds the enabled, verified audio requests for one import. The
// window and input hash come from the decoded PCM file itself
// (WholePCMInput), never from the container metadata. With a claimable stage
// set only capabilities a worker claims right now and whose model verifies are
// queued; without one every configured capability is queued (legacy rule). A
// capability whose model is unavailable is skipped; it must not block ASR. A
// recording whose PCM holds less than one millisecond skips audio analysis
// entirely and keeps ASR.
func (s *Service) audioPlans(ctx context.Context, recordingID, sourceSHA, pcmRel string, claimable map[string]bool) ([]audioPlan, error) {
	if s.audioCfg == nil || s.audio == nil {
		return nil, nil
	}
	durationMS, inputHash, err := audioanalysis.WholePCMInput(ctx, s.audioDir, pcmRel)
	if err != nil {
		if audioanalysis.ErrorCodeOf(err) == audioanalysis.CodeInputInvalid {
			return nil, nil
		}
		return nil, err
	}
	source := sourceSHA
	if len(source) != 64 {
		source = inputHash
	}
	plans := make([]audioPlan, 0, len(audioCapabilities))
	for _, capability := range audioCapabilities {
		if claimable != nil && !claimable["audio_"+capability] {
			continue
		}
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
