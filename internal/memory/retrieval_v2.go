package memory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
)

// SearchMemoryV2 performs bounded lexical retrieval with Chinese character
// bigrams and Latin/numeric tokens. It scores eligible records before applying
// the existing search surface's maximum result page size.
func (s *Store) SearchMemoryV2(ctx context.Context, q MemorySearch) (SearchPage, error) {
	if err := ctx.Err(); err != nil {
		return SearchPage{}, err
	}
	if q.Cursor != "" {
		return SearchPage{}, errors.New("v2 search does not support cursors")
	}
	queryTokens := lexicalTokens(q.Text)
	if len(queryTokens) == 0 {
		return SearchPage{}, errors.New("text is required")
	}
	scope := strings.ToLower(strings.TrimSpace(q.Scope))
	switch scope {
	case "", "both", "raw", "derived":
	default:
		return SearchPage{}, fmt.Errorf("invalid scope: %s", q.Scope)
	}
	// Project the eligible corpus once through the existing filtering and
	// provenance paths, then score terms in memory. This avoids per-gram
	// rescans and ensures irrelevant early IDs cannot crowd out a relevant hit.
	hits := []SearchHit{}
	if scope == "" || scope == "both" || scope == "derived" {
		notes, err := s.searchNotes(ctx, q, "")
		if err != nil {
			return SearchPage{}, err
		}
		for _, h := range notes {
			if q.ConversationID == "" || h.ConversationID == q.ConversationID {
				hits = append(hits, h)
			}
		}
	}
	if scope == "" || scope == "both" || scope == "raw" {
		turns, err := s.searchTurns(ctx, q, "")
		if err != nil {
			return SearchPage{}, err
		}
		for _, h := range turns {
			if q.ConversationID == "" || h.ConversationID == q.ConversationID {
				hits = append(hits, h)
			}
		}
	}
	docs := make([][]string, len(hits))
	df := map[string]int{}
	totalLen := 0
	for i, h := range hits {
		docs[i] = lexicalDocumentTokens(h.Text)
		totalLen += len(docs[i])
		seen := map[string]bool{}
		for _, tok := range docs[i] {
			if !seen[tok] {
				df[tok]++
				seen[tok] = true
			}
		}
	}
	avgLen := float64(totalLen) / math.Max(1, float64(len(hits)))
	for i := range hits {
		hits[i].Score = 0
		tf := map[string]int{}
		for _, tok := range docs[i] {
			tf[tok]++
		}
		score := 0.0
		querySeen := map[string]bool{}
		for _, tok := range queryTokens {
			if querySeen[tok] {
				continue
			}
			querySeen[tok] = true
			freq := tf[tok]
			if freq == 0 {
				continue
			}
			idf := math.Log(1 + (float64(len(hits)-df[tok])+0.5)/(float64(df[tok])+0.5))
			k1, b := 1.2, 0.75
			score += idf * (float64(freq) * (k1 + 1)) / (float64(freq) + k1*(1-b+b*float64(len(docs[i]))/math.Max(avgLen, 1)))
		}
		// Exact case-folded phrase matches get a modest deterministic boost.
		if score > 0 && strings.Contains(strings.ToLower(hits[i].Text), strings.ToLower(strings.TrimSpace(q.Text))) {
			score += 1.0
		}
		if score == 0 {
			continue
		}
		hits[i].Score = score
		hits[i].Why = "BM25 lexical match"
	}
	relevant := hits[:0]
	for _, h := range hits {
		if h.Score > 0 {
			relevant = append(relevant, h)
		}
	}
	hits = relevant
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if hits[i].Kind != hits[j].Kind {
			return searchKindRank(hits[i].Kind) < searchKindRank(hits[j].Kind)
		}
		return hits[i].ID < hits[j].ID
	})
	limit := clampPageLimit(q.Limit)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return SearchPage{Hits: hits}, nil
}

// lexicalTokens emits overlapping CJK bigrams, preserving single CJK runes
// for one-character queries, plus lower-cased contiguous Latin/numeric terms.
func lexicalTokens(s string) []string {
	runes := []rune(strings.ToLower(strings.TrimSpace(s)))
	out := []string{}
	for i := 0; i < len(runes); {
		if isCJK(runes[i]) {
			j := i + 1
			for j < len(runes) && isCJK(runes[j]) {
				j++
			}
			if j-i == 1 {
				out = append(out, string(runes[i]))
			} else {
				for k := i; k+1 < j; k++ {
					out = append(out, string(runes[k:k+2]))
				}
			}
			i = j
			continue
		}
		if unicode.IsLetter(runes[i]) || unicode.IsDigit(runes[i]) {
			j := i + 1
			for j < len(runes) && (unicode.IsLetter(runes[j]) || unicode.IsDigit(runes[j])) && !isCJK(runes[j]) {
				j++
			}
			out = append(out, string(runes[i:j]))
			i = j
			continue
		}
		i++
	}
	// A CJK string query also needs constituent unigrams, enabling single-char
	// matches while bigrams retain phrase relevance.
	if len(runes) == 1 && isCJK(runes[0]) {
		return []string{string(runes[0])}
	}
	return out
}

// lexicalDocumentTokens also emits CJK unigrams so a one-character query can
// match within a longer Chinese document without making multi-character
// queries recall on every constituent character.
func lexicalDocumentTokens(s string) []string {
	runes := []rune(strings.ToLower(s))
	out := lexicalTokens(s)
	for i := 0; i < len(runes); {
		if !isCJK(runes[i]) {
			i++
			continue
		}
		j := i + 1
		for j < len(runes) && isCJK(runes[j]) {
			j++
		}
		for k := i; k < j; k++ {
			out = append(out, string(runes[k]))
		}
		i = j
	}
	return out
}

func isCJK(r rune) bool { return (r >= 0x3400 && r <= 0x9fff) || (r >= 0xf900 && r <= 0xfaff) }
