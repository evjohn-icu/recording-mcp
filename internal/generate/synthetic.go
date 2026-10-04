package generate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Synthetic is a deterministic Generator used for tests and offline wiring. It
// needs no network: the number of drafts, their kinds, summaries and cited
// evidence turns are all derived from sha256(state), so identical inputs always
// produce identical output.
type Synthetic struct{}

// syntheticModel is the model identifier reported by Synthetic.
const syntheticModel = "synthetic"

// Generate implements Generator. It derives 0-2 drafts from the state hash and
// returns none when no evidence turn is available, because a draft without
// evidence would be rejected by the pipeline anyway.
func (Synthetic) Generate(_ context.Context, state string, evidenceTurnIDs []string) ([]Draft, string, error) {
	if len(evidenceTurnIDs) == 0 {
		return nil, syntheticModel, nil
	}
	sum := sha256.Sum256([]byte(state))
	n := int(sum[0] % 3) // 0, 1 or 2 drafts
	drafts := make([]Draft, 0, n)
	for i := range n {
		h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", state, i)))
		kind := Kinds[int(h[0])%len(Kinds)]
		drafts = append(drafts, Draft{
			Kind:               kind,
			Summary:            syntheticSummary(state, h),
			TopicTitle:         "合成主题 " + hex.EncodeToString(h[2:5]),
			EvidenceTurnIDs:    []string{evidenceTurnIDs[int(h[1])%len(evidenceTurnIDs)]},
			InterpretationNote: "synthetic deterministic draft",
		})
	}
	return drafts, syntheticModel, nil
}

// syntheticSummary builds a non-empty summary from the state, prefixed with a
// short hash tag so distinct drafts stay distinguishable.
func syntheticSummary(state string, h [sha256.Size]byte) string {
	flat := strings.Join(strings.Fields(state), " ")
	if flat == "" {
		flat = "空状态"
	}
	runes := []rune(flat)
	if len(runes) > 80 {
		runes = runes[:80]
	}
	return fmt.Sprintf("合成候选(%s)：%s", hex.EncodeToString(h[:4]), string(runes))
}
