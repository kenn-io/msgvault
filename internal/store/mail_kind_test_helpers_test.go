package store_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func addLocalMailKindLabelForTest(t *testing.T, st *store.Store, messageID int64, kind string) {
	t.Helper()
	var labelID int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO labels (source_id, name, label_type)
		VALUES (NULL, ?, 'auto') RETURNING id`), "mv:kind/"+kind).Scan(&labelID))
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO message_labels (message_id, label_id) VALUES (?, ?)`), messageID, labelID)
	require.NoError(t, err)
}
