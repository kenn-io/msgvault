package oauth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGrantCoversAnyScopeKeepsDraftOperationsSeparate(t *testing.T) {
	tests := []struct {
		name     string
		granted  []string
		accepted []string
		want     bool
	}{
		{name: "modify covers writes", granted: []string{ScopeGmailModify}, accepted: ScopesGmailDraftWrite, want: true},
		{name: "readonly covers send-as", granted: []string{ScopeGmailReadonly}, accepted: ScopesGmailSendAsList, want: true},
		{name: "settings basic covers send-as", granted: []string{ScopeGmailSettingsBasic}, accepted: ScopesGmailSendAsList, want: true},
		{name: "settings basic does not write", granted: []string{ScopeGmailSettingsBasic}, accepted: ScopesGmailDraftWrite, want: false},
		{name: "compose writes", granted: []string{ScopeGmailCompose}, accepted: ScopesGmailDraftWrite, want: true},
		{name: "empty", granted: nil, accepted: ScopesGmailDraftWrite, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, GrantCoversAnyScope(tt.granted, tt.accepted))
		})
	}
}
