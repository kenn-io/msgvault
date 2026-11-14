package api

import (
	"slices"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/authz"
)

func apiV1PolicyOperations(t *testing.T) map[string]*huma.Operation {
	t.Helper()
	operations := make(map[string]*huma.Operation)
	for path, item := range openAPIClientDocument().Paths {
		if !strings.HasPrefix(path, "/api/v1/") {
			continue
		}
		for _, op := range []*huma.Operation{item.Get, item.Put, item.Post, item.Delete, item.Options, item.Head, item.Patch, item.Trace} {
			if op == nil {
				continue
			}
			require.NotEmpty(t, op.OperationID, "%s", path)
			_, duplicate := operations[op.OperationID]
			assert.False(t, duplicate, "duplicate operation ID %s", op.OperationID)
			operations[op.OperationID] = op
		}
	}
	require.NotEmpty(t, operations)
	return operations
}

func TestEveryAPIV1OperationIsClassified(t *testing.T) {
	assert := assert.New(t)
	operations := apiV1PolicyOperations(t)
	for id, op := range operations {
		policy, ok := operationPolicies[id]
		if !assert.True(ok, "unclassified %s %s %s", id, op.Method, op.Path) {
			continue
		}
		_, known := authz.KnownPermission(string(policy.Classification))
		assert.True(known, "%s", id)
	}
	for id := range operationPolicies {
		assert.Contains(operations, id, "stale policy entry %s", id)
	}
	for _, public := range []string{"health", "headHealth", "daemonPing", "daemonIdentity", "daemonShutdown", "getSession", "loginSession", "logoutSession"} {
		assert.NotContains(operationPolicies, public)
	}
}

func TestOperationPolicyDefaultsDenyCapabilities(t *testing.T) {
	assert := assert.New(t)
	for _, id := range []string{"", "unknown", "getPersonProfile.extra", "GETMESSAGE", "*"} {
		policy := operationPolicy(id)
		assert.Equal(authz.PermissionArchiveAdmin, policy.Classification)
		assert.Empty(policy.AgentReadPermission)
		assert.False(policy.Delegated)
		assert.False(policy.RemoteRead)
		assert.False(policy.RemoteCollectionsWrite)
		assert.False(delegatedOperationAllowed(id))
		assert.False(remoteClientOperationAllowed(id, true))
	}
}

func TestLegacyOperationAdmissionIsPreserved(t *testing.T) {
	assert := assert.New(t)
	wantReads := delegatedReads("", 0, 0, "")
	wantRemoteRead := map[string]bool{
		"getHealth": true, "getCLIStats": true, "searchCLI": true, "listCLIAccounts": true, "getCLICacheStats": true,
		"getCLIMessage": true, "getCLIMessageOriginal": true, "getCLIMessageThread": true, "getCLIMessageRaw": true,
		"getCLIAttachment": true, "listCLICollections": true, "getCLICollection": true, "listCLIIdentities": true,
	}
	wantRemoteWrite := map[string]bool{
		"createCLICollection": true, "addCLICollectionSources": true, "removeCLICollectionSources": true, "deleteCLICollection": true,
	}
	operations := apiV1PolicyOperations(t)
	operations["unknown"] = nil
	for id := range operations {
		read, isRead := wantReads[id]
		assert.Equal(isRead || slices.Contains(allowedDelegatedOps, id), delegatedOperationAllowed(id), "%s", id)
		assert.Equal(wantRemoteRead[id], remoteClientOperationAllowed(id, false), "%s", id)
		assert.Equal(wantRemoteRead[id] || wantRemoteWrite[id], remoteClientOperationAllowed(id, true), "%s", id)
		assert.Equal(read.permission, operationPolicy(id).AgentReadPermission, "%s", id)
	}
}
