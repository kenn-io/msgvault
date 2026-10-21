package store_test

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"testing"
)

func TestPersonColdPolicyAdmissionRechecksSemanticInput(t *testing.T) {
	requirements := require.New(t)

	f := storetest.New(t)
	ctx := t.Context()
	id := mustPerson(t, f, "input@example.invalid", "Input Person")
	doc, err := f.Store.LoadPersonSemanticDocumentContext(ctx, id)
	requirements.NoError(err)
	work, err := f.Store.CaptureConversationPolicyWork(ctx)
	requirements.NoError(err)
	work = store.WithConversationPolicyPeople(work, []store.ConversationPolicyPersonVersion{{PersonID: id, Revision: doc.Revision}})
	requirements.NoError(store.CheckConversationPolicyAdmission(work))
	person, err := f.Store.GetPerson(id)
	requirements.NoError(err)
	_, err = f.Store.UpdatePersonDisplayNameContext(ctx, id, person.Revision, new("Changed Input"))
	requirements.NoError(err)
	assert.ErrorIs(t, store.CheckConversationPolicyAdmission(work), store.ErrConversationPolicyStale, "every HTTP retry uses the original person input, even without a room transition")
}
