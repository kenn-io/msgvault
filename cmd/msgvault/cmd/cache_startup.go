package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/daemon"
)

const daemonStartupCacheBuildEnv = "MSGVAULT_DAEMON_STARTUP_CACHE_BUILD"

type startupCacheBuildIntent string

const (
	startupCacheBuildIntentNone    startupCacheBuildIntent = ""
	startupCacheBuildIntentDefault startupCacheBuildIntent = "default"
	startupCacheBuildIntentFull    startupCacheBuildIntent = "full"
)

type startupCacheBuildOutcome string

const (
	startupCacheBuildOutcomeNone       startupCacheBuildOutcome = ""
	startupCacheBuildOutcomeFulfilled  startupCacheBuildOutcome = "fulfilled"
	startupCacheBuildOutcomeFailed     startupCacheBuildOutcome = "failed"
	startupCacheBuildOutcomeFatal      startupCacheBuildOutcome = "fatal"
	startupCacheBuildOutcomeUnconsumed startupCacheBuildOutcome = "unconsumed"
)

func startupCacheBuildIntentFromEnv() (startupCacheBuildIntent, error) {
	intent := startupCacheBuildIntent(os.Getenv(daemonStartupCacheBuildEnv))
	switch intent {
	case startupCacheBuildIntentNone,
		startupCacheBuildIntentDefault,
		startupCacheBuildIntentFull:
		return intent, nil
	default:
		return startupCacheBuildIntentNone,
			fmt.Errorf("invalid daemon startup cache build intent %q", intent)
	}
}

func withStartupCacheBuildIntent(base []string, intent startupCacheBuildIntent) []string {
	prefix := daemonStartupCacheBuildEnv + "="
	out := make([]string, 0, len(base)+1)
	replaced := false
	for _, entry := range base {
		if strings.HasPrefix(entry, prefix) {
			if !replaced && intent != startupCacheBuildIntentNone {
				out = append(out, prefix+string(intent))
				replaced = true
			}
			continue
		}
		out = append(out, entry)
	}
	if !replaced && intent != startupCacheBuildIntentNone {
		out = append(out, prefix+string(intent))
	}
	return out
}

func startupCacheBuildOutcomeFromRuntime(rt *DaemonRuntime) startupCacheBuildOutcome {
	if rt == nil || rt.Record.Metadata == nil {
		return startupCacheBuildOutcomeNone
	}
	return startupCacheBuildOutcome(rt.Record.Metadata[runtimeStartupCacheBuildOutcome])
}

// durableStartupCacheBuildOutcomePath identifies the one-shot result for an
// exact daemon process. A fatal required-DuckDB failure shuts the daemon down,
// so its normal runtime record can disappear before the parent polls again.
func durableStartupCacheBuildOutcomePath(dataDir string, rec daemon.RuntimeRecord) string {
	return filepath.Join(dataDir, fmt.Sprintf(
		".daemon.%d.%d.startup-cache-outcome",
		rec.PID, rec.StartedAt.UnixNano(),
	))
}

func writeDurableStartupCacheBuildOutcome(
	dataDir string,
	rec daemon.RuntimeRecord,
	outcome startupCacheBuildOutcome,
) error {
	path := durableStartupCacheBuildOutcomePath(dataDir, rec)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".startup-cache-outcome-*.tmp")
	if err != nil {
		return fmt.Errorf("create startup cache outcome: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure startup cache outcome: %w", err)
	}
	if _, err := tmp.WriteString(string(outcome) + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write startup cache outcome: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync startup cache outcome: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close startup cache outcome: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publish startup cache outcome: %w", err)
	}
	committed = true
	return nil
}

func consumeDurableStartupCacheBuildOutcome(
	dataDir string,
	rec daemon.RuntimeRecord,
) (startupCacheBuildOutcome, bool, error) {
	path := durableStartupCacheBuildOutcomePath(dataDir, rec)
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return startupCacheBuildOutcomeNone, false, nil
	}
	if err != nil {
		return startupCacheBuildOutcomeNone, false,
			fmt.Errorf("read startup cache outcome: %w", err)
	}
	outcome := startupCacheBuildOutcome(strings.TrimSpace(string(body)))
	if outcome != startupCacheBuildOutcomeFatal {
		return startupCacheBuildOutcomeNone, false,
			fmt.Errorf("invalid durable startup cache outcome %q", outcome)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return startupCacheBuildOutcomeNone, false,
			fmt.Errorf("consume startup cache outcome: %w", err)
	}
	return outcome, true, nil
}
