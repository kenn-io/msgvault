package cmd

import (
	"context"
	"go.kenn.io/msgvault/internal/store"
)

var serveAllowDeliveryPolicyWrites bool

func init() {
	serveCmd.Flags().BoolVar(&serveAllowDeliveryPolicyWrites, "allow-delivery-policy-writes", false, "Enable separate owner-authorized contact delivery policy edits")
}
func (a *storeAPIAdapter) GetDeliveryPolicyContext(ctx context.Context, q store.DeliveryPolicyQuery) (*store.DeliveryPolicyState, error) {
	return a.store.GetDeliveryPolicyContext(ctx, q)
}
func (a *storeAPIAdapter) SetDeliveryPolicyContext(ctx context.Context, w store.DeliveryPolicyWrite) (*store.DeliveryPolicyReceipt, error) {
	return a.store.SetDeliveryPolicyContext(ctx, w)
}
func (a *storeAPIAdapter) ClearDeliveryPolicyContext(ctx context.Context, w store.DeliveryPolicyWrite) (*store.DeliveryPolicyReceipt, error) {
	return a.store.ClearDeliveryPolicyContext(ctx, w)
}
