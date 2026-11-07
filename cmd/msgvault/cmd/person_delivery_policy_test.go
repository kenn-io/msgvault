package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonDeliveryPolicyCommands(t *testing.T) {
	const (
		addressDraft     = `{"person_uid":"person-example","endpoint":"alice@example.test","revision":3,"effective_policy":"draft_only","inheritance_source":"system_default","reason":"draft_required"}`
		addressSend      = `{"person_uid":"person-example","endpoint":"alice@example.test","revision":3,"stored_policy":"send_allowed","effective_policy":"send_allowed","inheritance_source":"contact_method","reason":"explicit_approval"}`
		addressApproved  = `{"person_uid":"person-example","endpoint":"alice@example.test","revision":4,"stored_policy":"send_allowed","effective_policy":"send_allowed","inheritance_source":"contact_method","reason":"explicit_approval"}`
		addressCleared   = `{"person_uid":"person-example","endpoint":"alice@example.test","revision":4,"effective_policy":"draft_only","inheritance_source":"system_default","reason":"draft_required"}`
		personDraft      = `{"person_uid":"person-example","revision":3,"effective_policy":"draft_only","inheritance_source":"system_default","reason":"draft_required"}`
		personSend       = `{"person_uid":"person-example","revision":3,"stored_policy":"send_allowed","effective_policy":"send_allowed","inheritance_source":"person_default","reason":"explicit_person_default"}`
		personApproved   = `{"person_uid":"person-example","revision":4,"stored_policy":"send_allowed","effective_policy":"send_allowed","inheritance_source":"person_default","reason":"explicit_person_default"}`
		personRestricted = `{"person_uid":"person-example","revision":4,"stored_policy":"draft_only","effective_policy":"draft_only","inheritance_source":"person_default","reason":"draft_required"}`
		personCleared    = `{"person_uid":"person-example","revision":4,"effective_policy":"draft_only","inheritance_source":"system_default","reason":"draft_required"}`
		bindingChanged   = `{"person_uid":"person-example","endpoint":"alice@example.test","revision":4,"stored_policy":"send_allowed","effective_policy":"draft_only","inheritance_source":"contact_method","reason":"binding_changed"}`
		ambiguousAddress = `{"person_uid":"person-example","endpoint":"alice@example.test","revision":4,"stored_policy":"send_allowed","effective_policy":"draft_only","inheritance_source":"contact_method","reason":"ambiguous_endpoint"}`
	)
	for _, test := range []struct {
		name, operation   string
		args              []string
		want              store.DeliveryPolicyWrite
		schema, wantError string
		human             bool
		wantReason        string
		wantSource        string
		before, after     string
	}{
		{name: "read JSON", operation: "read", args: []string{"person-example", "--endpoint", "alice@example.test", "--json"}, want: store.DeliveryPolicyWrite{PersonUID: "person-example", Endpoint: "alice@example.test"}, after: addressSend},
		{name: "read human", operation: "read", args: []string{"person-example", "--endpoint", "alice@example.test"}, want: store.DeliveryPolicyWrite{PersonUID: "person-example", Endpoint: "alice@example.test"}, human: true, after: addressSend, wantSource: "Address"},
		{name: "read human person default", operation: "read", args: []string{"person-example"}, want: store.DeliveryPolicyWrite{PersonUID: "person-example"}, human: true, after: personSend, wantSource: "Person"},
		{name: "read human changed binding", operation: "read", args: []string{"person-example", "--endpoint", "alice@example.test"}, want: store.DeliveryPolicyWrite{PersonUID: "person-example", Endpoint: "alice@example.test"}, human: true, after: bindingChanged, wantReason: "Address changed; approve again", wantSource: "Address"},
		{name: "read human ambiguous endpoint", operation: "read", args: []string{"person-example", "--endpoint", "alice@example.test"}, want: store.DeliveryPolicyWrite{PersonUID: "person-example", Endpoint: "alice@example.test"}, human: true, after: ambiguousAddress, wantReason: "Address shared by multiple people", wantSource: "Address"},
		{name: "set address", operation: "set", args: []string{"person-example", "send_allowed", "--endpoint", "alice@example.test", "--expected-revision", "3", "--reason", "Owner approval", "--json"}, want: store.DeliveryPolicyWrite{PersonUID: "person-example", Endpoint: "alice@example.test", ExpectedRevision: 3, Policy: store.DeliverySendAllowed, Reason: "Owner approval"}, before: addressDraft, after: addressApproved},
		{name: "set person default", operation: "set", args: []string{"person-example", "send_allowed", "--person-all-routes", "--expected-revision", "3", "--reason", "Owner approval", "--json"}, want: store.DeliveryPolicyWrite{PersonUID: "person-example", ExpectedRevision: 3, Policy: store.DeliverySendAllowed, ScopeAcknowledgement: "person_all_routes", Reason: "Owner approval"}, before: personDraft, after: personApproved},
		{name: "restrict person default", operation: "set", args: []string{"person-example", "draft_only", "--expected-revision", "3", "--reason", "Restrict", "--json"}, want: store.DeliveryPolicyWrite{PersonUID: "person-example", ExpectedRevision: 3, Policy: store.DeliveryDraftOnly, Reason: "Restrict"}, before: personSend, after: personRestricted},
		{name: "clear address", operation: "clear", args: []string{"person-example", "--endpoint", "alice@example.test", "--expected-revision", "3", "--reason", "Reset", "--json"}, want: store.DeliveryPolicyWrite{PersonUID: "person-example", Endpoint: "alice@example.test", ExpectedRevision: 3, Reason: "Reset"}, before: addressSend, after: addressCleared},
		{name: "clear person default", operation: "clear", args: []string{"person-example", "--person-all-routes", "--expected-revision", "3", "--reason", "Reset"}, want: store.DeliveryPolicyWrite{PersonUID: "person-example", ExpectedRevision: 3, ScopeAcknowledgement: "person_all_routes", Reason: "Reset"}, human: true, before: personSend, after: personCleared, wantSource: "Default"},
		{name: "invalid policy", operation: "set", args: []string{"person-example", "allow", "--expected-revision", "3", "--reason", "Owner approval"}, want: store.DeliveryPolicyWrite{PersonUID: "person-example", Policy: "allow", ExpectedRevision: 3, Reason: "Owner approval"}, wantError: store.ErrDeliveryPolicyInvalid.Error()},
		{name: "invalid revision", operation: "clear", args: []string{"person-example", "--expected-revision", "0", "--reason", "Reset"}, want: store.DeliveryPolicyWrite{PersonUID: "person-example", Reason: "Reset"}, wantError: store.ErrDeliveryPolicyInvalid.Error()},
		{name: "missing revision", operation: "set", args: []string{"person-example", "send_allowed", "--reason", "Owner approval"}, wantError: "expected-revision"},
		{name: "missing reason", operation: "clear", args: []string{"person-example", "--expected-revision", "3"}, wantError: "reason"},
		{name: "missing UID", operation: "read", wantError: "arg"},
		{name: "old daemon read", operation: "read", args: []string{"person-example"}, schema: "3.45.9", wantError: "daemon API schema 3.48.0 or newer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			var requests int
			schema := test.schema
			if schema == "" {
				schema = deliveryPolicyMinAPISchemaVersion
			}
			payload := test.after
			if test.operation != "read" {
				payload = `{"before":` + test.before + `,"after":` + test.after + `}`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/health" {
					_, _ = fmt.Fprintf(w, `{"status":"ok","api_schema_version":%q}`, schema)
					return
				}
				requests++
				assert.Equal(http.MethodPost, r.Method)
				assert.Equal("/api/v1/people/delivery-policy/"+test.operation, r.URL.Path)
				var body store.DeliveryPolicyWrite
				assert.NoError(json.NewDecoder(r.Body).Decode(&body))
				assert.Equal(test.want, body)
				if test.wantError == store.ErrDeliveryPolicyInvalid.Error() {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = fmt.Fprintf(w, `{"error":"invalid_policy","message":%q}`, store.ErrDeliveryPolicyInvalid.Error())
					return
				}
				_, _ = io.WriteString(w, payload)
			}))
			t.Cleanup(server.Close)
			ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
			command := newPersonDeliveryPolicyCommand(test.operation)
			command.SilenceErrors, command.SilenceUsage = true, true
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(io.Discard)
			command.SetArgs(append([]string{}, test.args...))
			err := command.ExecuteContext(ctx)
			if test.wantError != "" {
				require.ErrorContains(err, test.wantError)
				if test.wantError == store.ErrDeliveryPolicyInvalid.Error() {
					assert.Equal(1, requests)
				} else {
					assert.Zero(requests)
				}
				return
			}
			require.NoError(err)
			assert.Equal(1, requests)
			if test.human {
				header := "PERSON UID ENDPOINT REVISION STORED POLICY EFFECTIVE POLICY SOURCE"
				if test.wantReason != "" {
					header += " REASON"
				}
				lines := strings.SplitN(output.String(), "\n", 2)
				require.Len(lines, 2)
				assert.Equal(strings.Fields(header), strings.Fields(lines[0]))
				var after store.DeliveryPolicyState
				require.NoError(json.Unmarshal([]byte(test.after), &after))
				stored := "-"
				if after.StoredPolicy != nil {
					stored = string(*after.StoredPolicy)
				}
				assert.Equal(strings.Fields(fmt.Sprintf("%s %s %d %s %s %s %s", after.PersonUID, after.Endpoint, after.Revision, stored, after.EffectivePolicy, test.wantSource, test.wantReason)), strings.Fields(lines[1]))
			} else {
				assert.JSONEq(payload, output.String())
			}
		})
	}
}
