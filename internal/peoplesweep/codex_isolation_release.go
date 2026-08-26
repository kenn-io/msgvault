package peoplesweep

// releasedCodexAttestations is intentionally empty. Codex App Server v2
// exposes runtimeWorkspaceRoots, selectedCapabilityRoots, and a read-only
// sandbox but no enforceable readable-root allowlist, so no inspected
// executable digest can prove the packet-only containment boundary.
var releasedCodexAttestations = map[CodexReleaseKey]CodexAttestation{}
