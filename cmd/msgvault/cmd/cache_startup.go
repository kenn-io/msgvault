package cmd

import (
	"fmt"
	"os"
	"strings"
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
	for _, entry := range base {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	if intent != startupCacheBuildIntentNone {
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
