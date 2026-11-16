package agentgrant

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIssueRejectsRoleOnlyPermission(t *testing.T) {
	for position := range 3 {
		registry := NewRegistry()
		permissions := slices.Insert([]Permission{PermissionMessageRead, PermissionSearchRead}, position, Permission("archive.admin"))
		_, secret, _, err := registry.Issue("bounded", permissions, []SourceRef{{ID: 1, Type: "test", Identifier: "reader@example.test"}}, time.Time{})
		require.Error(t, err)
		assert.Empty(t, secret)
		assert.Empty(t, registry.List())
	}
}
