package peoplesweep

import "runtime"

// releasedCodexAttestations stays empty until the pinned artifact passes
// denied execution and filesystem-access probes and a real packet-only structured
// inference through the production launcher: Bubblewrap on compatible Linux,
// Seatbelt on compatible macOS.
var releasedCodexAttestations = map[CodexReleaseKey]CodexAttestation{}

// CodexReleaseAvailable reports whether this platform has a certified inference
// release. Enrollment must remain unavailable until users can use its credentials.
// Each launch still verifies the selected executable against the release registry.
func CodexReleaseAvailable() bool {
	return codexReleaseAvailable(releasedCodexAttestations)
}

// codexReleaseAvailable admits only entries certified for this OS and
// architecture, so a certification on one platform opens no other's gate.
func codexReleaseAvailable(registry map[CodexReleaseKey]CodexAttestation) bool {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return false
	}
	for key, attestation := range registry {
		if key.ExecutionBoundary == CodexExecutionBoundaryV1 && validReleasedCodexAttestation(key, attestation) {
			return true
		}
	}
	return false
}
