package peoplesweep

func CommitmentRequestForTest(window CommitmentWindow, maxOutputTokens int64) (StructuredRequest, error) {
	return commitmentRequest(window, maxOutputTokens)
}
