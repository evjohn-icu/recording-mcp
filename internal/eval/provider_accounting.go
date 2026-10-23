package eval

import (
	"context"
	"sync"

	"recordingmcp/internal/semantic"
)

// accountingProvider wraps the selected embedder without changing its
// configuration or vectors, and records actual Embed method invocations.
type accountingProvider struct {
	inner    semantic.Provider
	mu       sync.Mutex
	requests int
	items    int
	errors   int
}

func (p *accountingProvider) ID() string    { return p.inner.ID() }
func (p *accountingProvider) Model() string { return p.inner.Model() }
func (p *accountingProvider) Dims() int     { return p.inner.Dims() }
func (p *accountingProvider) Embed(ctx context.Context, texts []string) ([]semantic.Vector, error) {
	p.mu.Lock()
	p.requests++
	p.items += len(texts)
	p.mu.Unlock()
	vectors, err := p.inner.Embed(ctx, texts)
	if err != nil {
		p.mu.Lock()
		p.errors++
		p.mu.Unlock()
	}
	return vectors, err
}

func (p *accountingProvider) snapshot() (requests, items, errors int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests, p.items, p.errors
}
