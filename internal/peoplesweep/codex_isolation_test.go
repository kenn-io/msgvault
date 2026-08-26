package peoplesweep

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const codexIsolationFixtureVersion = "codex-cli 0.149.0"

type isolationCountingStarter struct {
	starts atomic.Int64
}

func (s *isolationCountingStarter) Start(
	context.Context,
	CodexExecutable,
	[]string,
	[]string,
	string,
) (RPCProcess, error) {
	s.starts.Add(1)
	return nil, errors.New("unexpected Codex process start")
}

type injectedReleasedCodexGate struct {
	registry map[CodexReleaseKey]CodexAttestation
}

func (g injectedReleasedCodexGate) Verify(
	ctx context.Context,
	executable string,
	expectedBoundary string,
) (CodexAttestation, error) {
	return verifyReleasedCodexIsolation(ctx, executable, expectedBoundary, g.registry)
}

func (g injectedReleasedCodexGate) ReverifyForLaunch(attestation CodexAttestation) error {
	return reverifyReleasedCodexIsolation(attestation, g.registry)
}

type replaceAfterVerifyCodexGate struct {
	t        *testing.T
	registry map[CodexReleaseKey]CodexAttestation
	bytes    []byte
}

type codexIsolationExecutableFixture struct {
	version string
	mode    string
	marker  string
	secret  string
}

func buildCodexIsolationExecutableFixture(
	t *testing.T,
	fixture codexIsolationExecutableFixture,
) (string, []byte) {
	t.Helper()
	filename := "codex-fixture"
	if runtime.GOOS == "windows" {
		filename += ".exe"
	}
	path := filepath.Join(t.TempDir(), filename)
	linkerValues := []string{
		"-X=main.versionBase64=" + base64.StdEncoding.EncodeToString([]byte(fixture.version)),
		"-X=main.mode=" + fixture.mode,
		"-X=main.markerBase64=" + base64.StdEncoding.EncodeToString([]byte(fixture.marker)),
		"-X=main.secret=" + fixture.secret,
	}
	command := exec.CommandContext(
		t.Context(), "go", "build", "-trimpath", "-ldflags", strings.Join(linkerValues, " "), "-o", path, ".",
	)
	command.Dir = filepath.Join("testdata", "codexfixture")
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	return path, contents
}

func (g replaceAfterVerifyCodexGate) Verify(
	ctx context.Context,
	executable string,
	expectedBoundary string,
) (CodexAttestation, error) {
	attestation, err := verifyReleasedCodexIsolation(ctx, executable, expectedBoundary, g.registry)
	if err != nil {
		return CodexAttestation{}, err
	}
	require.NoError(g.t, os.WriteFile(attestation.ExecutablePath, g.bytes, 0o700))
	return attestation, nil
}

func (g replaceAfterVerifyCodexGate) ReverifyForLaunch(attestation CodexAttestation) error {
	return reverifyReleasedCodexIsolation(attestation, g.registry)
}

func writeCodexIsolationFixture(t *testing.T, body string) (string, []byte) {
	t.Helper()
	contents := []byte("#!/bin/sh\n" + body)
	path := filepath.Join(t.TempDir(), "codex-fixture")
	require.NoError(t, os.WriteFile(path, contents, 0o700))
	return path, contents
}

func codexIsolationFixtureRegistry(contents []byte) map[CodexReleaseKey]CodexAttestation {
	digestBytes := sha256.Sum256(contents)
	digest := hex.EncodeToString(digestBytes[:])
	key := CodexReleaseKey{
		ExecutableSHA256:  digest,
		ExecutionBoundary: CodexExecutionBoundaryV1,
	}
	return map[CodexReleaseKey]CodexAttestation{
		key: {
			Version:           codexIsolationFixtureVersion,
			ExecutableSHA256:  digest,
			ExecutionBoundary: CodexExecutionBoundaryV1,
			LaunchArtifact:    CodexLaunchArtifactNativeStandaloneV1,
		},
	}
}

func codexIsolationTransportConfig(executable string) ProviderConfig {
	return ProviderConfig{
		Kind: ProviderCodexAppServer, Model: "gpt-test", ReasoningEffort: "high",
		RetentionPosture: "zero_data_retention", TrainingPosture: "no_training",
		AllowedSources: []SourceClass{SourceConversationText}, SourceSince: "2025-01-01",
		Executable: executable, ExecutionBoundary: CodexExecutionBoundaryV1,
	}
}

// TestCodexProviderIsUnavailableWithoutReleasedBoundary catches a local
// executable or configured version being treated as proof of containment.
func TestCodexProviderIsUnavailableWithoutReleasedBoundary(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "invoked")
	executable, _ := buildCodexIsolationExecutableFixture(t, codexIsolationExecutableFixture{
		version: codexIsolationFixtureVersion, mode: "marker", marker: marker,
	})

	attestation, err := NewReleasedCodexIsolationGate().Verify(
		t.Context(), executable, CodexExecutionBoundaryV1,
	)
	require.ErrorIs(t, err, ErrCodexIsolationUnreleased)
	assert.Empty(t, attestation)
	assert.NoFileExists(t, marker)
}

// TestCodexRegistryCheckPrecedesVersionExecution catches --version running
// before the digest/boundary pair is found in the compiled release registry.
func TestCodexRegistryCheckPrecedesVersionExecution(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "version-ran")
	tempRoot := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(tempRoot, []byte("fixture"), 0o600))
	executable, _ := buildCodexIsolationExecutableFixture(t, codexIsolationExecutableFixture{
		version: codexIsolationFixtureVersion, mode: "marker", marker: marker,
	})
	t.Setenv("TMPDIR", tempRoot)

	_, err := verifyReleasedCodexIsolation(
		t.Context(), executable, CodexExecutionBoundaryV1,
		map[CodexReleaseKey]CodexAttestation{},
	)
	require.ErrorIs(t, err, ErrCodexIsolationUnreleased)
	assert.NoFileExists(t, marker)
}

// TestCodexRegisteredScriptIsNotAReleasableArtifact catches a registry entry
// blessing a shebang, shim, or other launcher whose interpreter/resources are
// outside the verified snapshot.
func TestCodexRegisteredScriptIsNotAReleasableArtifact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no native shebang execution contract")
	}
	marker := filepath.Join(t.TempDir(), "registered-script-ran")
	executable, contents := writeCodexIsolationFixture(t,
		"printf invoked > '"+marker+"'\nprintf 'codex-cli 0.149.0\\n'\n")

	attestation, err := verifyReleasedCodexIsolation(
		t.Context(), executable, CodexExecutionBoundaryV1, codexIsolationFixtureRegistry(contents),
	)
	require.ErrorIs(t, err, ErrCodexIsolationUnreleased)
	assert.Empty(t, attestation)
	assert.NoFileExists(t, marker)
}

// TestCodexReleasedFixtureAttestsExactIdentity catches a registered digest
// being combined with caller-owned version, boundary, digest, or path data.
func TestCodexReleasedFixtureAttestsExactIdentity(t *testing.T) {
	checks := assert.New(t)
	marker := filepath.Join(t.TempDir(), "version-ran")
	executable, contents := buildCodexIsolationExecutableFixture(t, codexIsolationExecutableFixture{
		version: codexIsolationFixtureVersion, mode: "marker", marker: marker,
	})
	registry := codexIsolationFixtureRegistry(contents)

	attestation, err := verifyReleasedCodexIsolation(
		t.Context(), executable, CodexExecutionBoundaryV1, registry,
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, attestation.Close()) })
	wantDigestBytes := sha256.Sum256(contents)
	wantExecutable, err := filepath.EvalSymlinks(filepath.Clean(executable))
	require.NoError(t, err)
	wantExecutable, err = filepath.Abs(wantExecutable)
	require.NoError(t, err)
	checks.Equal(filepath.Clean(wantExecutable), attestation.ExecutablePath)
	checks.Equal(hex.EncodeToString(wantDigestBytes[:]), attestation.ExecutableSHA256)
	checks.Equal(codexIsolationFixtureVersion, attestation.Version)
	checks.Equal(CodexExecutionBoundaryV1, attestation.ExecutionBoundary)
	checks.Equal(CodexLaunchArtifactNativeStandaloneV1, attestation.LaunchArtifact)
	checks.FileExists(marker, "a registered digest must be version-checked")
}

// TestCodexVersionExecutesAcceptedSnapshotAfterSourceSwap catches Verify
// reopening the configured source path after its accepted digest was copied.
func TestCodexVersionExecutesAcceptedSnapshotAfterSourceSwap(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	verifiedMarker := filepath.Join(t.TempDir(), "verified-version-ran")
	swappedMarker := filepath.Join(t.TempDir(), "swapped-version-ran")
	executable, contents := buildCodexIsolationExecutableFixture(t, codexIsolationExecutableFixture{
		version: codexIsolationFixtureVersion, mode: "marker", marker: verifiedMarker,
	})
	attestation, err := prepareReleasedCodexIsolation(
		executable, CodexExecutionBoundaryV1, codexIsolationFixtureRegistry(contents),
	)
	must.NoError(err)
	t.Cleanup(func() { must.NoError(attestation.Close()) })
	must.NoError(os.Rename(executable, executable+".accepted-source"))
	must.NoError(os.WriteFile(executable, []byte(
		"#!/bin/sh\nprintf swapped > '"+swappedMarker+"'\nprintf 'codex-cli 0.149.0\\n'\n",
	), 0o700))

	version, err := codexExecutableVersion(t.Context(), attestation.VerifiedExecutable())
	must.NoError(err)
	checks.Equal(codexIsolationFixtureVersion, version)
	checks.FileExists(verifiedMarker)
	checks.NoFileExists(swappedMarker)
}

// TestCodexStartExecutesReverifiedSnapshotAfterSourceSwap catches Start
// reopening the configured source or an ancestor after accepted reverification.
func TestCodexStartExecutesReverifiedSnapshotAfterSourceSwap(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	verifiedMarker := filepath.Join(t.TempDir(), "verified-app-server-ran")
	swappedMarker := filepath.Join(t.TempDir(), "swapped-app-server-ran")
	executable, contents := buildCodexIsolationExecutableFixture(t, codexIsolationExecutableFixture{
		version: codexIsolationFixtureVersion, marker: verifiedMarker,
	})
	registry := codexIsolationFixtureRegistry(contents)
	attestation, err := verifyReleasedCodexIsolation(
		t.Context(), executable, CodexExecutionBoundaryV1, registry,
	)
	must.NoError(err)
	verifiedPath := attestation.VerifiedExecutable().verifiedPath
	must.NoError(reverifyReleasedCodexIsolation(attestation, registry))
	must.NoError(os.Rename(executable, executable+".reverified-source"))
	must.NoError(os.WriteFile(executable, []byte(
		"#!/bin/sh\nprintf swapped > '"+swappedMarker+"'\n",
	), 0o700))

	process, err := NewCodexCommandStarter().Start(
		t.Context(), attestation.VerifiedExecutable(), []string{"app-server"},
		scrubCodexEnvironment(os.Environ()), t.TempDir(),
	)
	must.NoError(err)
	must.NoError(process.Stdin().Close())
	must.NoError(process.Wait())
	checks.FileExists(verifiedMarker)
	checks.NoFileExists(swappedMarker)
	must.NoError(attestation.Close())
	checks.NoFileExists(verifiedPath, "attestation cleanup must remove the owned snapshot")
}

// TestCodexVersionTimeoutKillsHangingFixture catches factory verification
// waiting forever or returning executable output/path content on timeout.
func TestCodexVersionTimeoutKillsHangingFixture(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	lateMarker := filepath.Join(t.TempDir(), "late-hanging-marker")
	const secret = "HANGING_VERSION_SECRET"
	executable, contents := buildCodexIsolationExecutableFixture(t, codexIsolationExecutableFixture{
		version: codexIsolationFixtureVersion, mode: "hang", marker: lateMarker, secret: secret,
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
	checks.NotContains(err.Error(), secret)
	checks.NotContains(err.Error(), executable)
	time.Sleep(700 * time.Millisecond)
	checks.NoFileExists(lateMarker, "timed-out version process group must be terminated")
}

// TestCodexVersionTimeoutClosesDescendantStdout catches a successful parent
// leaving verification blocked on a descendant that inherited the stdout pipe.
func TestCodexVersionTimeoutClosesDescendantStdout(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	lateMarker := filepath.Join(t.TempDir(), "late-descendant-marker")
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
	checks.NotContains(err.Error(), lateMarker)
	time.Sleep(time.Second)
	checks.NoFileExists(lateMarker, "stdout-holding descendant must be terminated")
}

// TestCodexReleasedFixtureRequiresExactVersion catches a matching digest being
// accepted after its --version output drifts from the registry-owned value.
func TestCodexReleasedFixtureRequiresExactVersion(t *testing.T) {
	executable, contents := buildCodexIsolationExecutableFixture(t, codexIsolationExecutableFixture{
		version: "codex-cli 0.150.0",
	})
	registry := codexIsolationFixtureRegistry(contents)

	attestation, err := verifyReleasedCodexIsolation(
		t.Context(), executable, CodexExecutionBoundaryV1, registry,
	)
	require.ErrorIs(t, err, ErrCodexIsolationUnreleased)
	assert.Empty(t, attestation)
}

// TestCodexFactoryFailsBeforeStartingProcess catches construction of an
// unreleased Codex transport reaching the App Server process boundary.
func TestCodexFactoryFailsBeforeStartingProcess(t *testing.T) {
	executable, _ := buildCodexIsolationExecutableFixture(t, codexIsolationExecutableFixture{
		version: codexIsolationFixtureVersion,
	})
	starter := &isolationCountingStarter{}

	transport, err := NewStructuredTransport(
		codexIsolationTransportConfig(executable), nil, starter, NewReleasedCodexIsolationGate(),
	)
	require.ErrorIs(t, err, ErrCodexIsolationUnreleased)
	assert.Nil(t, transport)
	assert.Zero(t, starter.starts.Load())
}

// TestCodexReverifyRejectsExecutableReplacement catches an executable being
// swapped at its attested absolute path between Verify and CommandStarter.Start.
func TestCodexReverifyRejectsExecutableReplacement(t *testing.T) {
	must := require.New(t)
	executable, contents := buildCodexIsolationExecutableFixture(t, codexIsolationExecutableFixture{
		version: codexIsolationFixtureVersion,
	})
	gate := replaceAfterVerifyCodexGate{
		t: t, registry: codexIsolationFixtureRegistry(contents),
		bytes: []byte("#!/bin/sh\nprintf 'replacement 0.149.0\\n'\n"),
	}
	starter := &isolationCountingStarter{}
	transport, err := NewCodexAppServerTransport(
		codexIsolationTransportConfig(executable), starter, gate,
	)
	must.NoError(err)
	config := Config{Enabled: true, Provider: codexIsolationTransportConfig(executable)}
	config.ApplyDefaults()
	profile, err := config.Profile()
	must.NoError(err)
	request := StructuredRequest{
		ProgramID: "replacement-test", ProgramVersion: "1",
		Sources:   []SourceDescriptor{{Class: SourceConversationText, ObservedOn: "2026-08-23"}},
		InputText: `{"synthetic":"packet"}`, SchemaName: "empty",
		JSONSchema:      []byte(`{"type":"object","properties":{},"additionalProperties":false}`),
		MaxOutputTokens: 16,
	}
	prepared, err := transport.PrepareJSON(profile, request)
	must.NoError(err)
	_, err = transport.GeneratePreparedJSON(t.Context(), profile, "", prepared)
	must.ErrorIs(err, ErrCodexIsolationUnreleased)
	assert.Zero(t, starter.starts.Load(), "replacement must fail at reverify before Start")
}
