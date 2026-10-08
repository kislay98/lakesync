package engine

import (
	"context"
	"fmt"
	"sync"

	"lakesync/internal/conn"
	"lakesync/internal/dest"
	"lakesync/internal/source"
	"lakesync/internal/store"
)

// pool shares one Source per source connection and one Destination per
// destination connection across runners (MongoDB connection limits).
type pool struct {
	deps  Deps
	mu    sync.Mutex
	srcs  map[string]*sharedSource
	dests map[string]*sharedDest
}

type sharedSource struct {
	src  source.Source
	refs int
	rev  string // updated_at of the connection row, to reopen on edits
}

type sharedDest struct {
	dst   dest.Destination
	alias string
	refs  int
	rev   string
}

func newPool(d Deps) *pool {
	return &pool{deps: d, srcs: map[string]*sharedSource{}, dests: map[string]*sharedDest{}}
}

func (p *pool) source(ctx context.Context, id string) (source.Source, error) {
	c, err := p.deps.Store.GetConnection(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("source connection: %w", err)
	}
	rev := c.UpdatedAt.String()
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.srcs[id]; ok && s.rev == rev {
		s.refs++
		return s.src, nil
	}
	src, err := conn.OpenSource(ctx, p.deps.Secrets, c)
	if err != nil {
		return nil, err
	}
	if old, ok := p.srcs[id]; ok && old.refs == 0 {
		_ = old.src.Close()
	}
	p.srcs[id] = &sharedSource{src: src, refs: 1, rev: rev}
	return src, nil
}

func (p *pool) releaseSource(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.srcs[id]; ok {
		s.refs--
		if s.refs <= 0 {
			_ = s.src.Close()
			delete(p.srcs, id)
		}
	}
}

func (p *pool) destination(ctx context.Context, id string) (dest.Destination, string, error) {
	c, err := p.deps.Store.GetConnection(ctx, id)
	if err != nil {
		return nil, "", fmt.Errorf("destination connection: %w", err)
	}
	rev := c.UpdatedAt.String()
	p.mu.Lock()
	defer p.mu.Unlock()
	if d, ok := p.dests[id]; ok && d.rev == rev {
		d.refs++
		return d.dst, d.alias, nil
	}
	dst, err := conn.OpenDestination(ctx, p.deps.Secrets, c)
	if err != nil {
		return nil, "", err
	}
	alias, err := conn.Attach(ctx, p.deps.Query, p.deps.Secrets, c)
	if err != nil {
		_ = dst.Close()
		return nil, "", fmt.Errorf("attach destination to DuckDB: %w", err)
	}
	if old, ok := p.dests[id]; ok && old.refs == 0 {
		_ = old.dst.Close()
	}
	p.dests[id] = &sharedDest{dst: dst, alias: alias, refs: 1, rev: rev}
	return dst, alias, nil
}

func (p *pool) releaseDestination(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d, ok := p.dests[id]; ok {
		d.refs--
		if d.refs <= 0 {
			_ = d.dst.Close()
			delete(p.dests, id)
		}
	}
}

func (p *pool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, s := range p.srcs {
		_ = s.src.Close()
		delete(p.srcs, id)
	}
	for id, d := range p.dests {
		_ = d.dst.Close()
		delete(p.dests, id)
	}
}

var _ = store.ErrNotFound
