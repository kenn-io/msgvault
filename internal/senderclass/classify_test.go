package senderclass

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassify(t *testing.T) {
	for _, tt := range []struct {
		name string
		e    Evidence
		want Result
	}{
		{"source ownership", Evidence{true, AuthFail, RelationshipContact}, Result{Self, SourceOwned}},
		{"spoofed contact", Evidence{false, AuthFail, RelationshipContact}, Result{Unverified, AuthFailed}},
		{"missing auth", Evidence{false, AuthNone, RelationshipContact}, Result{Unverified, NoAuthEvidence}},
		{"unknown auth", Evidence{false, "future", RelationshipKnown}, Result{Unverified, NoAuthEvidence}},
		{"contact", Evidence{false, AuthPass, RelationshipContact}, Result{Contact, CuratedPerson}},
		{"known", Evidence{false, AuthPlatform, RelationshipKnown}, Result{Known, OwnerSentTo}},
		{"dmarc", Evidence{false, AuthPass, RelationshipNone}, Result{Verified, DMARCPass}},
		{"platform", Evidence{false, AuthPlatform, RelationshipNone}, Result{Verified, Platform}},
		{"invalid relationship", Evidence{false, AuthPass, "future"}, Result{Unverified, InvalidRelationship}},
		{"zero evidence", Evidence{}, Result{Unverified, NoAuthEvidence}},
	} {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, Classify(tt.e)) })
	}
}

func TestFloors(t *testing.T) {
	assert := assert.New(t)
	ladder := []Class{Unverified, Verified, Known, Contact}
	for i, c := range ladder {
		for j, floor := range ladder {
			assert.Equal(i >= j, c.AtLeast(floor), "%s >= %s", c, floor)
		}
		assert.True(c.AtLeast(Any))
		assert.False(c.AtLeast("future"))
	}
	for _, floor := range append(ladder, Any, Self, "future") {
		assert.False(Self.AtLeast(floor))
		assert.False(Class("future").AtLeast(floor))
		assert.False(Any.AtLeast(floor))
	}
	for _, floor := range []Class{Contact, Known, Verified, Any} {
		got, err := ParseFloor(string(floor))
		require.NoError(t, err)
		assert.Equal(floor, got)
	}
	for _, floor := range []string{"", "self", "unverified", "KNOWN", " known", "future"} {
		_, err := ParseFloor(floor)
		assert.Error(err)
	}
}

func FuzzAuthenticationGate(f *testing.F) {
	for _, auth := range []string{"pass", "platform", "none", "fail", "", "future"} {
		for _, relationship := range []string{"contact", "known", "none", "", "future"} {
			f.Add(auth, relationship, false)
			f.Add(auth, relationship, true)
		}
	}
	f.Fuzz(func(t *testing.T, auth, relationship string, owned bool) {
		result := Classify(Evidence{owned, AuthState(auth), Relationship(relationship)})
		if owned {
			assert.Equal(t, Result{Self, SourceOwned}, result)
			assert.False(t, result.Class.AtLeast(Any))
			return
		}
		assert.NotEqual(t, Self, result.Class)
		if result.Class.AtLeast(Verified) {
			assert.Contains(t, []string{"pass", "platform"}, auth)
			assert.Contains(t, []string{"contact", "known", "none"}, relationship)
		}
	})
}
