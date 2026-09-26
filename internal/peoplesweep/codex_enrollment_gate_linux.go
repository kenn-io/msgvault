//go:build linux

package peoplesweep

import "context"

// codexEnrollmentIsolationGate admits the reviewed native artifact only for
// the narrow enrollment client. It does not populate the inference registry.
type codexEnrollmentIsolationGate struct{}

var codexEnrollmentAttestations = map[CodexReleaseKey]CodexAttestation{
	{ExecutableSHA256: "78a11f06e0a2dda42d13fba1d50dc62e8cbdb2d5f69789722f4d4d99b5cdbe30", ExecutionBoundary: CodexExecutionBoundaryV1}: {
		Version: "codex-cli 0.156.0", ExecutableSHA256: "78a11f06e0a2dda42d13fba1d50dc62e8cbdb2d5f69789722f4d4d99b5cdbe30",
		ExecutionBoundary: CodexExecutionBoundaryV1, LaunchArtifact: CodexLaunchArtifactNativeStandaloneV1,
	},
}

func (codexEnrollmentIsolationGate) Verify(ctx context.Context, executable, boundary string) (CodexAttestation, error) {
	return verifyReleasedCodexIsolation(ctx, executable, boundary, codexEnrollmentAttestations)
}

func (codexEnrollmentIsolationGate) ReverifyForLaunch(attestation CodexAttestation) error {
	return reverifyReleasedCodexIsolation(attestation, codexEnrollmentAttestations)
}
