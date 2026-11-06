package daemonclient

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestDeliveryPolicyClientPreservesStateAndReceipts(t *testing.T) {
	inheritedJSON := `{"person_uid":"person-7","revision":2,"effective_policy":"draft_only","inheritance_source":"default","reason":"Default policy","endpoint":null}`
	approvedJSON := `{"person_uid":"person-7","revision":3,"stored_policy":"send_allowed","effective_policy":"send_allowed","inheritance_source":"endpoint","reason":"Approved address","endpoint":"peer@example.test"}`
	client := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "application/json")
		var body string
		switch r.URL.Path {
		case "/api/v1/people/delivery-policy/read":
			body = inheritedJSON
		case "/api/v1/people/delivery-policy/set":
			body = `{"before":` + inheritedJSON + `,"after":` + approvedJSON + `,"audit":{"actor":"owner","reason":"Approved address","created_at":"2026-11-06T12:00:00Z","scope":"endpoint","before_policy":null,"after_policy":"send_allowed","contact_point_id":42}}`
		case "/api/v1/people/delivery-policy/clear":
			body = `{"before":` + inheritedJSON + `,"after":` + inheritedJSON + `}`
		default:
			http.NotFound(w, r)
			return
		}
		_, err := w.Write([]byte(body))
		assert.NoError(t, err)
	})

	inherited := store.DeliveryPolicyState{
		PersonUID: "person-7", Revision: 2, EffectivePolicy: store.DeliveryDraftOnly,
		InheritanceSource: "default", Reason: "Default policy",
	}
	state, err := client.GetDeliveryPolicy(t.Context(), store.DeliveryPolicyQuery{PersonUID: "person-7"})
	require.NoError(t, err)
	assert.Equal(t, &inherited, state)

	receipt, err := client.SetDeliveryPolicy(t.Context(), store.DeliveryPolicyWrite{
		PersonUID: "person-7", Endpoint: "peer@example.test", ExpectedRevision: 2,
		Policy: store.DeliverySendAllowed, Reason: "Approved address",
	})
	require.NoError(t, err)
	assert.Equal(t, &store.DeliveryPolicyReceipt{
		Before: inherited,
		After: store.DeliveryPolicyState{
			PersonUID: "person-7", Revision: 3, StoredPolicy: new(store.DeliverySendAllowed), EffectivePolicy: store.DeliverySendAllowed,
			InheritanceSource: "endpoint", Reason: "Approved address", Endpoint: "peer@example.test",
		},
		Audit: &store.DeliveryPolicyAudit{
			Actor: "owner", Reason: "Approved address", CreatedAt: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
			Scope: "endpoint", AfterPolicy: new(store.DeliverySendAllowed), ContactPointID: new(int64(42)),
		},
	}, receipt)

	receipt, err = client.ClearDeliveryPolicy(t.Context(), store.DeliveryPolicyWrite{
		PersonUID: "person-7", ExpectedRevision: 2, Reason: "Already inherited",
	})
	require.NoError(t, err)
	assert.Equal(t, &store.DeliveryPolicyReceipt{Before: inherited, After: inherited}, receipt)
}
