package whatsapp

import (
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestAndroidImportBindingFailureFailsSyncWithoutPanic(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	failWhatsAppImportRunBindingAfterAttempt(t, st)
	path := createGroupParticipantsImportFixture(t, nil)
	opts := DefaultOptions()
	opts.Phone = "+12025550123"
	importer := NewImporter(st, nil)
	var importErr error
	panicValue := recoverWhatsAppImportPanic(func() {
		_, importErr = importer.Import(t.Context(), path, opts)
	})

	assert.Nil(panicValue, "Android import should return the binding error without panicking")
	assert.ErrorContains(importErr, "bind Android WhatsApp import: invalid_import_run")
	assertWhatsAppLatestSyncFailed(t, st, assert, require)
}

func TestAppleImportBindingFailureFailsSyncWithoutPanic(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	failWhatsAppImportRunBindingAfterAttempt(t, st)
	path := createAppleChatFixture(t)
	createAppleLIDFixture(t, filepath.Dir(path))
	opts := DefaultOptions()
	opts.Phone = "+15555550100"
	importer := NewImporter(st, nil)
	var importErr error
	panicValue := recoverWhatsAppImportPanic(func() {
		_, importErr = importer.Import(t.Context(), path, opts)
	})

	assert.Nil(panicValue, "Apple import should return the binding error without panicking")
	assert.ErrorContains(importErr, "bind Apple WhatsApp import: invalid_import_run")
	assertWhatsAppLatestSyncFailed(t, st, assert, require)
}

func failWhatsAppImportRunBindingAfterAttempt(t *testing.T, st *store.Store) {
	t.Helper()
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION fail_whatsapp_import_run_binding() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN UPDATE import_runs SET completed=TRUE WHERE id=NEW.import_run_id; RETURN NEW; END $$`)
		Require.NoError(t, err)
		_, err = st.DB().Exec(`CREATE TRIGGER fail_whatsapp_import_run_binding AFTER INSERT ON import_run_attempts
			FOR EACH ROW EXECUTE FUNCTION fail_whatsapp_import_run_binding()`)
		Require.NoError(t, err)
		return
	}
	_, err := st.DB().Exec(`CREATE TRIGGER fail_whatsapp_import_run_binding AFTER INSERT ON import_run_attempts
		BEGIN UPDATE import_runs SET completed=TRUE WHERE id=NEW.import_run_id; END`)
	Require.NoError(t, err)
}

func assertWhatsAppLatestSyncFailed(t *testing.T, st *store.Store, assert *Assert.Assertions, require *Require.Assertions) {
	t.Helper()
	var status string
	require.NoError(st.DB().QueryRow(`SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status))
	assert.Equal(store.SyncStatusFailed, status)
}

func recoverWhatsAppImportPanic(run func()) (panicValue any) {
	defer func() { panicValue = recover() }()
	run()
	return nil
}
