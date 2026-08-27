//go:build windows

package peoplesweep

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexWindowsSnapshotPreservesExecutableSuffix(t *testing.T) {
	must := require.New(t)
	executable, _ := buildCodexIsolationExecutableFixture(t, codexIsolationExecutableFixture{
		version: codexIsolationFixtureVersion,
	})
	verified, _, err := snapshotCodexExecutable(executable)
	must.NoError(err)
	t.Cleanup(func() { must.NoError(verified.Close()) })
	assert.Equal(t, ".exe", filepath.Ext(verified.path))
}

func TestCodexWindowsVersionTimeoutTerminatesDescendantJob(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	lateMarker := filepath.Join(t.TempDir(), "windows-descendant-marker")
	executable, contents := buildCodexIsolationExecutableFixture(t, codexIsolationExecutableFixture{
		version: codexIsolationFixtureVersion, mode: "descendant", marker: lateMarker,
	})
	attestation, err := prepareReleasedCodexIsolation(
		executable, CodexExecutionBoundaryV1, codexIsolationFixtureRegistry(contents),
	)
	must.NoError(err)
	t.Cleanup(func() { must.NoError(attestation.Close()) })

	started := time.Now()
	_, err = codexExecutableVersion(t.Context(), attestation.VerifiedExecutable())
	must.Error(err)
	checks.Less(time.Since(started), 900*time.Millisecond)
	time.Sleep(time.Second)
	checks.NoFileExists(lateMarker, "the Job Object must terminate the stdout-holding descendant")
}
