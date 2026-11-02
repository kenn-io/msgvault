package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateMetaActions(t *testing.T) {
	for _, tt := range []struct {
		name             string
		actions, allowed []string
		want             string
	}{
		{"read only", nil, []string{"send"}, ""},
		{"known actions", []string{"send", "human_agent"}, []string{"send", "human_agent"}, ""},
		{"comment actions", []string{"public_reply", "hide", "delete"}, []string{"public_reply", "private_reply", "hide", "delete", "block"}, ""},
		{"unknown", []string{"reply"}, []string{"send"}, `meta_messages[2].actions has unknown or duplicate action "reply"`},
		{"duplicate", []string{"send", "send"}, []string{"send"}, `meta_messages[2].actions has unknown or duplicate action "send"`},
		{"empty action", []string{""}, []string{"send"}, `meta_messages[2].actions has unknown or duplicate action ""`},
		{"case sensitive", []string{"SEND"}, []string{"send"}, `meta_messages[2].actions has unknown or duplicate action "SEND"`},
		{"no allowed actions", []string{"send"}, nil, `meta_messages[2].actions has unknown or duplicate action "send"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMetaActions("meta_messages", 2, tt.actions, tt.allowed...)
			if tt.want == "" {
				assert.NoError(t, err)
			} else {
				assert.EqualError(t, err, tt.want)
			}
		})
	}
}

func TestActionsAllow(t *testing.T) {
	assert.False(t, ActionsAllow(nil, "send"))
	assert.False(t, ActionsAllow([]string{"send"}, "human_agent"))
	assert.True(t, ActionsAllow([]string{"send", "human_agent"}, "human_agent"))
	assert.False(t, ActionsAllow([]string{"send"}, "SEND"))
}
