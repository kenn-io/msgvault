package peoplesweep

import "runtime"

// releasedCodexAttestations stays empty until the pinned artifact passes
// negative read, write, and egress probes and a real packet-only structured
// inference through the production launcher on compatible Linux.
var releasedCodexAttestations = map[CodexReleaseKey]CodexAttestation{}

// CodexReleaseAvailable reports whether this platform has a certified inference
// release. Enrollment must remain unavailable until users can use its credentials.
// Each launch still verifies the selected executable against the release registry.
func CodexReleaseAvailable() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	for key, attestation := range releasedCodexAttestations {
		if key.ExecutionBoundary == CodexExecutionBoundaryV1 && validReleasedCodexAttestation(key, attestation) {
			return true
		}
	}
	return false
}
