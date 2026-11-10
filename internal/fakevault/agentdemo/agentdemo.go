// Package agentdemo builds a deterministic synthetic archive through production importers.
package agentdemo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/msgvault/internal/fakevault"
	"go.kenn.io/msgvault/internal/store"
)

const Scenario = "agent-demo"
const ScenarioVersion = 1

var DefaultAsOf = time.Date(2026, 6, 15, 16, 0, 0, 0, time.UTC)

type Options struct {
	Dir      string
	Seed     uint64
	AsOf     time.Time
	Progress func(chapter string)
}

func GenerateDir(ctx context.Context, opts Options) (m *Manifest, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Dir == "" {
		return nil, fmt.Errorf("agent-demo: output directory is required")
	}
	path := filepath.Join(opts.Dir, "msgvault.db")
	if err = fakevault.CheckNewTarget(path); err != nil {
		return nil, err
	}
	if err = os.MkdirAll(opts.Dir, 0700); err != nil {
		return nil, fmt.Errorf("create output: %w", err)
	}
	if err = fakevault.InitSchema(ctx, path); err != nil {
		return nil, err
	}
	st, err := store.OpenContext(ctx, path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := st.Close(); err == nil {
			err = closeErr
		}
	}()
	m, err = Generate(ctx, st, filepath.Join(opts.Dir, "attachments"), opts)
	if err != nil {
		return nil, err
	}
	if err = m.Write(opts.Dir); err != nil {
		return nil, err
	}
	return m, nil
}
func Generate(ctx context.Context, st *store.Store, attachmentsDir string, opts Options) (*Manifest, error) {
	if opts.AsOf.IsZero() {
		opts.AsOf = DefaultAsOf
	}
	opts.AsOf = opts.AsOf.UTC()
	if err := os.MkdirAll(attachmentsDir, 0700); err != nil {
		return nil, err
	}
	b := newBuilder(st, attachmentsDir, opts)
	for _, ch := range chapters {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if opts.Progress != nil {
			opts.Progress(ch.name)
		}
		if err := ch.run(ctx, b); err != nil {
			return nil, fmt.Errorf("agent-demo %s: %w", ch.name, err)
		}
	}
	if err := b.finalize(ctx); err != nil {
		return nil, fmt.Errorf("agent-demo finalize: %w", err)
	}
	return b.m, nil
}
