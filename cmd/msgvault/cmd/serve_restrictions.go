package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/store"
)

func (a *storeAPIAdapter) AddAgentRestriction(ctx context.Context, r store.AgentRestriction) (store.AgentRestriction, error) {
	return a.store.AddAgentRestriction(ctx, r)
}
func (a *storeAPIAdapter) ListAgentRestrictions(ctx context.Context) ([]store.AgentRestriction, error) {
	return a.store.ListAgentRestrictions(ctx)
}
func (a *storeAPIAdapter) RemoveAgentRestriction(ctx context.Context, id int64) error {
	return a.store.RemoveAgentRestriction(ctx, id)
}
func (a *storeAPIAdapter) CheckAgentRestrictions(ctx context.Context, id int64) ([]store.AgentRestriction, error) {
	return a.store.CheckAgentRestrictions(ctx, id)
}
func (a *storeAPIAdapter) AgentRestrictionTaint(ctx context.Context) (bool, error) {
	return a.store.AgentRestrictionTaint(ctx)
}
func (a *storeAPIAdapter) SetAgentRestrictionTaint(ctx context.Context, on bool) error {
	return a.store.SetAgentRestrictionTaint(ctx, on)
}
