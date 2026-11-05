package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"go.kenn.io/kit/embedlocal"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/visual"
)

type localEmbeddingsController struct {
	gate      api.OperationGate
	state     *invocation
	store     *store.Store
	dbPath    string
	handle    *vectorInitHandle
	server    *api.Server
	scheduler *scheduler.Scheduler
	runner    api.CLIRunner
	openers   []visual.StreamOpener
	inspect   func(context.Context, embedlocal.Options) (embedlocal.Status, error)
	install   func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error)
	endpoint  func(context.Context, embedlocal.Options) (string, error)
}

func (c *localEmbeddingsController) Inspect(ctx context.Context) (api.LocalEmbeddingsStatus, error) {
	dir, err := localEmbeddingsDir()
	if err != nil {
		return api.LocalEmbeddingsStatus{}, err
	}
	status, err := c.inspect(ctx, embedlocal.Options{DataDir: dir})
	if err != nil {
		return api.LocalEmbeddingsStatus{}, err
	}
	c.handle.mu.Lock()
	cfg := c.state.cfg.Vector
	if c.handle.vf != nil {
		cfg = c.handle.vf.Cfg
	}
	c.handle.mu.Unlock()
	provider := ""
	if cfg.Enabled {
		switch {
		case cfg.Embeddings.LocalOnly:
			provider = "builtin"
		case isOllamaEmbeddingEndpoint(cfg.Embeddings.Endpoint):
			provider = "ollama"
		default:
			provider = "configured"
		}
	}
	return api.LocalEmbeddingsStatus{Enabled: cfg.Enabled, Provider: provider, Compatible: cfg.Embeddings.LocalCompatible(),
		OllamaAvailable: status.OllamaAvailable, DownloadBytes: status.DownloadBytes, Phase: "idle"}, nil
}

func isOllamaEmbeddingEndpoint(endpoint string) bool {
	return strings.TrimRight(endpoint, "/") == "http://127.0.0.1:11434/v1" || strings.TrimRight(endpoint, "/") == "http://localhost:11434/v1"
}

func (c *localEmbeddingsController) Enable(ctx context.Context, builtin bool, report func(api.LocalEmbeddingsStatus)) error {
	if !c.handle.WaitContext(ctx) {
		return ctx.Err()
	}
	if c.gate != nil {
		release, ok := c.gate.BeginWorkContext(ctx)
		if !ok {
			return errors.New("the daemon is stopping")
		}
		defer release()
	}
	snapshot, err := config.ReadConfigFile(c.state.cfg.ConfigFilePath())
	if err != nil {
		return err
	}
	loaded, err := c.state.cfg.ReloadConfigFile(snapshot)
	if err != nil {
		return err
	}
	// A configured lane changes provider only through Use built-in instead.
	if loaded.Vector.Enabled && !builtin && !loaded.Vector.Embeddings.LocalOnly && !isOllamaEmbeddingEndpoint(loaded.Vector.Embeddings.Endpoint) {
		return nil
	}
	dir, err := localEmbeddingsDir()
	if err != nil {
		return err
	}
	available, err := c.inspect(ctx, embedlocal.Options{DataDir: dir})
	if err != nil {
		return err
	}
	var configKeys toml.MetaData
	configKeys, err = toml.Decode(string(snapshot.Content), &map[string]any{})
	if err != nil {
		return fmt.Errorf("read local search settings: %w", err)
	}
	backend, unavailable := setupVectorBackend(loaded)
	useOllama := !builtin && available.OllamaAvailable && !loaded.Vector.Embeddings.LocalOnly
	detection := setupDetection{backend: backend, backendUnavailable: unavailable, configKeys: configKeys, localEmbeddings: !useOllama}
	if useOllama {
		detection.embeddingEndpoint, err = c.endpoint(ctx, embedlocal.Options{DataDir: dir})
		if err != nil {
			return err
		}
		detection.ollamaEndpoint = detection.embeddingEndpoint
	}
	// This explicit action also enables a disabled, previously configured lane.
	candidate := *loaded
	candidate.Vector.Enabled = false
	if useOllama {
		candidate.Vector.Embeddings.Endpoint = ""
		candidate.Vector.Embeddings.LocalOnly = false
		if !candidate.Vector.Embeddings.LocalCompatible() {
			candidate.Vector.Embeddings = vector.EmbeddingsConfig{}
		}
	}
	lane := planTextSearch(&candidate, detection)
	if lane.Action != planActionEnable {
		return errors.New(lane.Reason)
	}
	if err := config.ValidateConfigTableEdits(snapshot, lane.edits); err != nil {
		return err
	}
	if !useOllama {
		installation, err := c.install(ctx, embedlocal.InstallOptions{DataDir: dir,
			Confirm: func(context.Context, int64) (bool, error) { return true, nil },
			Progress: func(done, total int64) {
				report(api.LocalEmbeddingsStatus{Phase: "downloading", DoneBytes: done, TotalBytes: total})
			},
		})
		if err != nil {
			return err
		}
		if _, err := c.endpoint(ctx, embedlocal.Options{DataDir: installation.DataDir, Executable: installation.Executable, SkipOllama: true}); err != nil {
			return err
		}
	}
	saved, err := config.EditConfigTables(loaded.ConfigFilePath(), snapshot.ETag, lane.edits)
	if err != nil {
		return err
	}
	nextConfig, err := loaded.ReloadConfigFile(saved)
	if err != nil {
		return err
	}
	// Apply only this lane to the running daemon; other saved changes remain pending.
	runtimeConfig := *c.state.cfg
	c.handle.mu.Lock()
	if c.handle.vf != nil {
		runtimeConfig.Vector = c.handle.vf.Cfg
	}
	c.handle.mu.Unlock()
	runtimeConfig.Vector.Enabled = true
	runtimeConfig.Vector.Backend = nextConfig.Vector.Backend
	runtimeConfig.Vector.Embeddings = nextConfig.Vector.Embeddings
	runtimeConfig.Vector.Embed.Schedule = nextConfig.Vector.Embed.Schedule
	state := *c.state
	state.cfg = &runtimeConfig
	nextCtx := withInvocation(ctx, &state)
	vecCfg, err := resolvedVectorConfig(nextCtx, c.store, runtimeConfig.Vector)
	if err != nil {
		return err
	}
	vecCfg, err = resolveLocalVectorConfig(nextCtx, vecCfg)
	if err != nil {
		return err
	}
	var vf *vectorFeatures
	if c.handle.vf == nil {
		vf, err = setupVectorFeaturesForRun(nextCtx, c.store, c.dbPath, false, c.openers...)
	} else {
		vf, err = c.handle.vf.configureText(nextCtx, &state, vecCfg)
	}
	if err != nil {
		return err
	}
	if vf == nil {
		return errors.New("local search is unavailable in this build")
	}
	c.handle.mu.Lock()
	c.handle.vf = vf
	c.handle.mu.Unlock()
	publishTextVectorFeatures(nextCtx, c.store, c.server, c.scheduler, vf, &state)
	report(api.LocalEmbeddingsStatus{Phase: "indexing"})
	fingerprint := vf.Cfg.GenerationFingerprint()
	building, err := vf.Backend.BuildingGeneration(ctx)
	if err != nil {
		return err
	}
	// Keep the initial build on the published configuration while settings remain editable.
	file, err := os.CreateTemp("", "msgvault-local-search-*.toml")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	encodeErr := toml.NewEncoder(file).Encode(struct {
		Data   config.DataConfig `toml:"data"`
		Vector vector.Config     `toml:"vector"`
	}{runtimeConfig.Data, runtimeConfig.Vector})
	if err := errors.Join(encodeErr, file.Close()); err != nil {
		return err
	}
	run := func(args ...string) error {
		args = append(args, "--config", file.Name())
		return c.runner.RunCLICommand(nextCtx, api.CLIRunRequest{Args: args}, nil)
	}
	if building != nil && building.Fingerprint != fingerprint {
		if err := run("embeddings", "retire", strconv.FormatInt(int64(building.ID), 10), "--yes"); err != nil {
			return err
		}
		building = nil
	}
	args := []string{"embeddings", "build", "--yes"}
	if building == nil {
		active, err := vf.Backend.ActiveGeneration(ctx)
		if err != nil && !errors.Is(err, vector.ErrNoActiveGeneration) {
			return fmt.Errorf("read active index: %w", err)
		}
		if errors.Is(err, vector.ErrNoActiveGeneration) || active.Fingerprint != fingerprint {
			args = append(args, "--full-rebuild")
		}
	}
	if err := run(args...); err != nil {
		return err
	}
	publishTextVectorFeatures(nextCtx, c.store, c.server, c.scheduler, vf, &state)
	return nil
}
