package loops

import (
	"go.kenn.io/msgvault/internal/freshness"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

type StatusResponse struct {
	State     store.LoopDerivationState `json:"state"`
	Freshness freshness.Report          `json:"freshness"`
}

type RunResponse struct {
	Disposition scheduler.JobDisposition `json:"disposition"`
}

type SuppressionListResponse struct {
	Items []Suppression `json:"items"`
}
