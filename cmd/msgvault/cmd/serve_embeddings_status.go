package cmd

import "go.kenn.io/msgvault/internal/vector"

func (a *schedulerAdapter) EmbeddingStatus() vector.EmbeddingStatus {
	if a == nil || a.scheduler == nil {
		return vector.EmbeddingStatus{}
	}
	return a.scheduler.EmbeddingStatus()
}
