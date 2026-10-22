// context-bench compares the pure-Go HNSW candidate index with an exact cosine
// oracle over deterministic synthetic vectors. It performs no provider calls.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"recordingmcp/internal/semantic"
)

type result struct {
	Items          int     `json:"items"`
	Dims           int     `json:"dims"`
	Queries        int     `json:"queries"`
	BuildMS        float64 `json:"build_ms"`
	ExactP50MS     float64 `json:"exact_p50_ms"`
	ExactP95MS     float64 `json:"exact_p95_ms"`
	HNSWP50MS      float64 `json:"hnsw_p50_ms"`
	HNSWP95MS      float64 `json:"hnsw_p95_ms"`
	Recall10       float64 `json:"recall_at_10"`
	Recall40       float64 `json:"recall_at_40"`
	HeapMB         float64 `json:"heap_mb"`
	RSSMB          float64 `json:"rss_mb,omitempty"`
	ExactOnly      bool    `json:"exact_only,omitempty"`
	HNSWM          int     `json:"hnsw_m,omitempty"`
	EFConstruction int     `json:"ef_construction,omitempty"`
	EFSearch       int     `json:"ef_search,omitempty"`
}

func main() {
	procs := runtime.NumCPU()
	if procs > 12 {
		procs = 12
	}
	runtime.GOMAXPROCS(procs)
	sizesFlag := flag.String("sizes", "10000,100000,1000000", "comma-separated item counts")
	dimsFlag := flag.String("dims", "256,768", "comma-separated dimensions")
	queries := flag.Int("queries", 100, "timed queries per configuration")
	warmup := flag.Int("warmup", 10, "warmup queries")
	exactOnly := flag.Bool("exact-only", false, "measure exact oracle without constructing HNSW")
	output := flag.String("output", "", "optional JSON report path")
	flag.Parse()
	sizes, err := ints(*sizesFlag)
	if err != nil {
		fatal(err)
	}
	dims, err := ints(*dimsFlag)
	if err != nil {
		fatal(err)
	}
	results := make([]result, 0, len(sizes)*len(dims))
	for _, d := range dims {
		for _, n := range sizes {
			r, err := run(n, d, *queries, *warmup, *exactOnly)
			if err != nil {
				fatal(err)
			}
			results = append(results, r)
			runtime.GC()
			debug.FreeOSMemory()
		}
	}
	var w io.Writer = os.Stdout
	if *output != "" {
		f, err := os.Create(*output)
		if err != nil {
			fatal(err)
		}
		defer f.Close()
		w = f
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(results); err != nil {
		fatal(err)
	}
}
func ints(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		n, e := strconv.Atoi(strings.TrimSpace(part))
		if e != nil || n <= 0 {
			return nil, fmt.Errorf("invalid positive integer %q", part)
		}
		out = append(out, n)
	}
	return out, nil
}
func run(n, dims, queries, warmup int, exactOnly bool) (result, error) {
	if queries <= 0 || warmup < 0 {
		return result{}, fmt.Errorf("queries must be positive and warmup nonnegative")
	}
	rng := rand.New(rand.NewSource(int64(n*31 + dims)))
	var idx *semantic.HNSW
	if !exactOnly {
		idx = semantic.NewHNSW(dims)
	}
	vectors := make([]semantic.Vector, n)
	started := time.Now()
	for i := 0; i < n; i++ {
		v := make(semantic.Vector, dims)
		var norm float64
		for j := range v {
			x := rng.NormFloat64()
			v[j] = float32(x)
			norm += x * x
		}
		scale := float32(1 / math.Sqrt(norm))
		for j := range v {
			v[j] *= scale
		}
		vectors[i] = v
		if idx != nil {
			if err := idx.Add(semantic.HNSWItem{Kind: "turn", ID: strconv.Itoa(i), Vector: v}); err != nil {
				return result{}, err
			}
		}
	}
	build := time.Since(started).Seconds() * 1000
	count := queries + warmup
	qs := make([]semantic.Vector, count)
	for i := range qs {
		v := append(semantic.Vector(nil), vectors[rng.Intn(n)]...)
		if i%2 == 1 {
			for j := range v {
				v[j] += float32(rng.NormFloat64() * 0.02)
			}
		}
		qs[i] = v
	}
	var exactTimes, hnswTimes []float64
	recall10, recall40 := 0.0, 0.0
	for i, q := range qs {
		start := time.Now()
		exact := exactTop(vectors, q, 40)
		et := time.Since(start).Seconds() * 1000
		ht := 0.0
		var got []semantic.HNSWItem
		if idx != nil {
			start = time.Now()
			got = idx.Search(q, 40, 2048)
			ht = time.Since(start).Seconds() * 1000
		}
		if i >= warmup {
			exactTimes = append(exactTimes, et)
			if idx != nil {
				hnswTimes = append(hnswTimes, ht)
				recall10 += recall(exact[:10], got, 10)
				recall40 += recall(exact[:40], got, 40)
			}
		}
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	r := result{Items: n, Dims: dims, Queries: queries, BuildMS: build, ExactP50MS: percentile(exactTimes, .5), ExactP95MS: percentile(exactTimes, .95), HNSWP50MS: percentile(hnswTimes, .5), HNSWP95MS: percentile(hnswTimes, .95), Recall10: recall10 / float64(max(queries, 1)), Recall40: recall40 / float64(max(queries, 1)), HeapMB: float64(m.HeapAlloc) / (1024 * 1024), RSSMB: processRSSMB(), ExactOnly: exactOnly}
	if !exactOnly {
		r.HNSWM = semantic.HNSWM
		r.EFConstruction = semantic.HNSWEFConstruction
		r.EFSearch = 2048
	}
	return r, nil
}
func exactTop(vs []semantic.Vector, q semantic.Vector, k int) []string {
	type scored struct {
		id string
		s  float64
	}
	top := make([]scored, 0, k)
	for i, v := range vs {
		s, _ := semantic.Cosine(q, v)
		x := scored{strconv.Itoa(i), s}
		at := len(top)
		for at > 0 && top[at-1].s < s {
			at--
		}
		if at < k {
			top = append(top, scored{})
			copy(top[at+1:], top[at:])
			top[at] = x
			if len(top) > k {
				top = top[:k]
			}
		}
	}
	out := make([]string, len(top))
	for i, x := range top {
		out[i] = x.id
	}
	return out
}
func recall(exact []string, got []semantic.HNSWItem, k int) float64 {
	if k > len(exact) {
		k = len(exact)
	}
	want := map[string]bool{}
	for _, id := range exact[:k] {
		want[id] = true
	}
	found := 0
	for _, x := range got {
		if want[x.ID] {
			found++
		}
	}
	return float64(found) / float64(k)
}
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	ys := append([]float64(nil), xs...)
	sortFloat(ys)
	i := int(float64(len(ys)-1) * p)
	return ys[i]
}
func sortFloat(x []float64) {
	for i := 1; i < len(x); i++ {
		v := x[i]
		j := i
		for j > 0 && x[j-1] > v {
			x[j] = x[j-1]
			j--
		}
		x[j] = v
	}
}
func processRSSMB() float64 {
	b, e := os.ReadFile("/proc/self/status")
	if e != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			var kb float64
			_, _ = fmt.Sscanf(line, "VmRSS: %f", &kb)
			return kb / 1024
		}
	}
	return 0
}
func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(2) }
