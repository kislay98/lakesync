// Package faulty wraps another source type and injects failures on demand. It
// exists for the end-to-end tests (brief section 15: "inject a history-lost
// error through a fault-injection wrapper around the Source") and is only
// registered when LAKESYNC_ENABLE_FAULT_INJECTION=true.
//
// Faults are requested by creating files in LAKESYNC_FAULT_DIR:
//
//	history_lost.<namespace>.<name>   -> the next Stream.Next returns core.ErrHistoryLost (file is consumed)
//	transient.<namespace>.<name>      -> the next Stream.Next returns a transient error (file is consumed)
package faulty

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"lakesync/internal/core"
	"lakesync/internal/source"
)

// TypeName is the registered type: the wrapped type with a "+faults" suffix.
const TypeName = "mongodb+faults"

// EnvEnable turns registration on; EnvDir is where fault files are looked up.
const (
	EnvEnable = "LAKESYNC_ENABLE_FAULT_INJECTION"
	EnvDir    = "LAKESYNC_FAULT_DIR"
)

func init() {
	if !strings.EqualFold(os.Getenv(EnvEnable), "true") {
		return
	}
	source.Register(source.Descriptor{
		Type:        TypeName,
		Title:       "MongoDB (fault injection, tests only)",
		Description: "The MongoDB source wrapped with on-demand failures for end-to-end tests.",
		Schema:      lazySchema("mongodb"),
		New: func(ctx context.Context, config map[string]any, secrets map[string]string) (source.Source, error) {
			inner, err := source.Open(ctx, "mongodb", config, secrets)
			if err != nil {
				return nil, err
			}
			return &Source{Source: inner, dir: os.Getenv(EnvDir)}, nil
		},
		Redact: func(config map[string]any) map[string]any {
			if d, err := source.Lookup("mongodb"); err == nil && d.Redact != nil {
				return d.Redact(config)
			}
			return config
		},
		Parse: func(text string) (map[string]any, map[string]string, error) {
			d, err := source.Lookup("mongodb")
			if err != nil || d.Parse == nil {
				return nil, nil, errors.New("mongodb type not registered")
			}
			return d.Parse(text)
		},
	})
}

func lazySchema(typ string) []byte {
	if d, err := source.Lookup(typ); err == nil {
		return d.Schema
	}
	return []byte(`{"type":"object","properties":{}}`)
}

// Source wraps a real source.
type Source struct {
	source.Source
	dir string
}

// Stream wraps the inner iterator with fault checks.
func (s *Source) Stream(ctx context.Context, e core.Entity, from core.Bookmark) (source.EventIterator, error) {
	if err := s.consume("stream_open", e); err != nil {
		return nil, err
	}
	it, err := s.Source.Stream(ctx, e, from)
	if err != nil {
		return nil, err
	}
	return &iterator{EventIterator: it, s: s, e: e}, nil
}

type iterator struct {
	source.EventIterator
	s *Source
	e core.Entity
}

func (it *iterator) Next(ctx context.Context) (*core.ChangeEvent, error) {
	if err := it.s.consume("next", it.e); err != nil {
		return nil, err
	}
	return it.EventIterator.Next(ctx)
}

// consume looks for a fault file for the entity and returns the matching error,
// deleting the file so the fault fires once.
func (s *Source) consume(_ string, e core.Entity) error {
	if s.dir == "" {
		return nil
	}
	for _, f := range []struct {
		prefix string
		err    error
	}{
		{"history_lost", core.ErrHistoryLost},
		{"transient", core.Transient(fmt.Errorf("injected transient failure"))},
	} {
		p := filepath.Join(s.dir, f.prefix+"."+e.String())
		if _, err := os.Stat(p); err == nil {
			_ = os.Remove(p)
			return f.err
		}
	}
	return nil
}
