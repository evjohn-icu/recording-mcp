package semantic

// This file contains a small, deterministic HNSW implementation used by the
// optional rebuildable semantic sidecar. It intentionally has no cgo or
// SQLite-driver dependency.
import (
	"container/heap"
	"encoding/gob"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
)

const (
	HNSWM              = 32
	HNSWEFConstruction = 200
)

type HNSWItem struct {
	Kind, ID, ConversationID, ContentHash string
	Vector                                Vector
}
type hnswNode struct {
	Item  HNSWItem
	Links [][]int
}
type HNSW struct {
	Dims            int
	Nodes           []hnswNode
	Entry, MaxLevel int
}

func NewHNSW(dims int) *HNSW { return &HNSW{Dims: dims, Entry: -1, MaxLevel: -1} }
func deterministicLevel(key string) int {
	var x uint64 = 14695981039346656037
	for i := 0; i < len(key); i++ {
		x ^= uint64(key[i])
		x *= 1099511628211
	}
	level := 0
	for level < 12 && x&3 == 0 {
		level++
		x >>= 2
	}
	return level
}
func (h *HNSW) Add(item HNSWItem) error {
	if h.Dims <= 0 || len(item.Vector) != h.Dims {
		return fmt.Errorf("semantic: HNSW vector dimension mismatch")
	}
	item.Vector = append(Vector(nil), item.Vector...)
	level := deterministicLevel(item.Kind + "\x00" + item.ID)
	idx := len(h.Nodes)
	node := hnswNode{Item: item, Links: make([][]int, level+1)}
	if h.Entry < 0 {
		h.Nodes = append(h.Nodes, node)
		h.Entry = idx
		h.MaxLevel = level
		return nil
	}
	h.Nodes = append(h.Nodes, node)
	cur := h.Entry
	for l := h.MaxLevel; l > level; l-- {
		cur = h.greedy(item.Vector, cur, l)
	}
	for l := min(level, h.MaxLevel); l >= 0; l-- {
		candidates := h.searchLayer(item.Vector, []int{cur}, HNSWEFConstruction, l)
		if len(candidates) > HNSWM {
			candidates = candidates[:HNSWM]
		}
		for _, c := range candidates {
			h.Nodes[idx].Links[l] = append(h.Nodes[idx].Links[l], c)
			h.Nodes[c].Links[l] = append(h.Nodes[c].Links[l], idx)
			h.prune(c, l)
		}
		if len(candidates) > 0 {
			cur = candidates[0]
		}
	}
	if level > h.MaxLevel {
		h.Entry = idx
		h.MaxLevel = level
	}
	return nil
}
func (h *HNSW) prune(idx, level int) {
	links := h.Nodes[idx].Links[level]
	sort.Slice(links, func(i, j int) bool {
		return cosineFast(h.Nodes[idx].Item.Vector, h.Nodes[links[i]].Item.Vector) > cosineFast(h.Nodes[idx].Item.Vector, h.Nodes[links[j]].Item.Vector)
	})
	if len(links) > HNSWM {
		links = links[:HNSWM]
	}
	h.Nodes[idx].Links[level] = links
}
func cosineFast(a, b Vector) float64 {
	var dot, aa, bb float64
	for i, x := range a {
		y := b[i]
		dot += float64(x * y)
		aa += float64(x * x)
		bb += float64(y * y)
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return dot / math.Sqrt(aa*bb)
}

type candidateHeap []neighbor

func (h candidateHeap) Len() int           { return len(h) }
func (h candidateHeap) Less(i, j int) bool { return h[i].score > h[j].score }
func (h candidateHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *candidateHeap) Push(x any)        { *h = append(*h, x.(neighbor)) }
func (h *candidateHeap) Pop() any          { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }

type bestHeap []neighbor

func (h bestHeap) Len() int           { return len(h) }
func (h bestHeap) Less(i, j int) bool { return h[i].score < h[j].score }
func (h bestHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *bestHeap) Push(x any)        { *h = append(*h, x.(neighbor)) }
func (h *bestHeap) Pop() any          { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }

type neighbor struct {
	id    int
	score float64
}

func (h *HNSW) greedy(q Vector, entry, level int) int {
	best := entry
	score := cosineFast(q, h.Nodes[best].Item.Vector)
	for changed := true; changed; {
		changed = false
		if level >= len(h.Nodes[best].Links) {
			break
		}
		for _, n := range h.Nodes[best].Links[level] {
			s := cosineFast(q, h.Nodes[n].Item.Vector)
			if s > score {
				best = n
				score = s
				changed = true
			}
		}
	}
	return best
}

func (h *HNSW) searchLayer(q Vector, entries []int, ef, level int) []int {
	visited := make(map[int]struct{}, ef*4)
	best := make(bestHeap, 0, ef)
	candidates := make(candidateHeap, 0, ef)
	for _, id := range entries {
		s := cosineFast(q, h.Nodes[id].Item.Vector)
		n := neighbor{id, s}
		heap.Push(&best, n)
		heap.Push(&candidates, n)
		visited[id] = struct{}{}
	}
	for candidates.Len() > 0 {
		cur := heap.Pop(&candidates).(neighbor)
		if best.Len() >= ef && cur.score < best[0].score {
			break
		}
		if level >= len(h.Nodes[cur.id].Links) {
			continue
		}
		for _, id := range h.Nodes[cur.id].Links[level] {
			if _, ok := visited[id]; ok {
				continue
			}
			visited[id] = struct{}{}
			s := cosineFast(q, h.Nodes[id].Item.Vector)
			if best.Len() < ef || s > best[0].score {
				n := neighbor{id, s}
				heap.Push(&candidates, n)
				heap.Push(&best, n)
				if best.Len() > ef {
					heap.Pop(&best)
				}
			}
		}
	}
	ordered := make([]neighbor, best.Len())
	for i := range ordered {
		ordered[i] = heap.Pop(&best).(neighbor)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].score > ordered[j].score })
	ids := make([]int, len(ordered))
	for i, n := range ordered {
		ids[i] = n.id
	}
	return ids
}
func (h *HNSW) Search(q Vector, k, ef int) []HNSWItem {
	if h == nil || h.Entry < 0 || len(q) != h.Dims || k <= 0 {
		return nil
	}
	if ef < k {
		ef = k
	}
	cur := h.Entry
	for l := h.MaxLevel; l > 0; l-- {
		cur = h.greedy(q, cur, l)
	}
	ids := h.searchLayer(q, []int{cur}, ef, 0)
	if len(ids) > k {
		ids = ids[:k]
	}
	out := make([]HNSWItem, len(ids))
	for i, id := range ids {
		out[i] = h.Nodes[id].Item
		out[i].Vector = nil
	}
	return out
}

type sidecarFile struct {
	Version           int
	ProviderID, Model string
	Dims              int
	Fingerprint       string
	Index             *HNSW
}

func SaveHNSWSidecar(path string, file sidecarFile) error {
	if path == "" {
		return errors.New("semantic: sidecar path required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	err = gob.NewEncoder(f).Encode(file)
	if e := f.Sync(); err == nil {
		err = e
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
func LoadHNSWSidecar(path, provider, model, fingerprint string, dims int) (*HNSW, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var file sidecarFile
	if err := gob.NewDecoder(f).Decode(&file); err != nil {
		return nil, err
	}
	if file.Version != 1 || file.ProviderID != provider || file.Model != model || file.Dims != dims || file.Fingerprint != fingerprint || file.Index == nil {
		return nil, errors.New("semantic: sidecar fingerprint mismatch")
	}
	return file.Index, nil
}
