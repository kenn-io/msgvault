package store_test

import (
	"database/sql"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested test scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested test scopes.
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Database triggers observe transaction ownership, including when Events is off.
func TestSourceLifetimeMutationsFenceIdentity(t *testing.T) {
	for _, field := range []string{"identifier", "oauth_app"} {
		t.Run(field, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			f := storetest.New(t)
			_, err := f.Store.DB().Exec(`INSERT INTO archive_metadata(key,value) VALUES ('synthetic_source_identity_fence','0')`)
			require.NoError(err)
			if f.Store.IsPostgreSQL() {
				_, err = f.Store.DB().Exec(`CREATE FUNCTION synthetic_observe_source_identity() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.key='identity_revision' THEN UPDATE archive_metadata SET value=txid_current()::text WHERE key='synthetic_source_identity_fence'; END IF; RETURN NEW; END $$`)
				require.NoError(err)
				_, err = f.Store.DB().Exec(`CREATE TRIGGER synthetic_observe_source_identity AFTER UPDATE ON archive_metadata FOR EACH ROW EXECUTE FUNCTION synthetic_observe_source_identity()`)
				require.NoError(err)
				_, err = f.Store.DB().Exec(`CREATE FUNCTION synthetic_require_source_identity() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF (SELECT value FROM archive_metadata WHERE key='synthetic_source_identity_fence')<>txid_current()::text THEN RAISE EXCEPTION 'source update requires identity fence in this transaction'; END IF; RETURN NEW; END $$`)
				require.NoError(err)
				_, err = f.Store.DB().Exec(`CREATE TRIGGER synthetic_require_source_identity BEFORE UPDATE ON sources FOR EACH ROW EXECUTE FUNCTION synthetic_require_source_identity()`)
				require.NoError(err)
			} else {
				_, err = f.Store.DB().Exec(`CREATE TRIGGER synthetic_observe_source_identity AFTER UPDATE ON archive_metadata WHEN NEW.key='identity_revision' BEGIN UPDATE archive_metadata SET value='1' WHERE key='synthetic_source_identity_fence'; END`)
				require.NoError(err)
				_, err = f.Store.DB().Exec(`CREATE TRIGGER synthetic_require_source_identity BEFORE UPDATE ON sources BEGIN SELECT CASE WHEN (SELECT value FROM archive_metadata WHERE key='synthetic_source_identity_fence')='1' THEN NULL ELSE RAISE(ABORT,'source update requires identity fence') END; END`)
				require.NoError(err)
			}
			if field == "identifier" {
				err = f.Store.UpdateSourceIdentifier(f.Source.ID, "replacement@example.net")
			} else {
				err = f.Store.UpdateSourceOAuthApp(f.Source.ID, sql.NullString{String: "synthetic-app", Valid: true})
			}
			require.NoError(err)
			var identifier string
			var app sql.NullString
			require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT identifier,oauth_app FROM sources WHERE id=?`), f.Source.ID).Scan(&identifier, &app))
			if field == "identifier" {
				assert.Equal("replacement@example.net", identifier)
			} else {
				assert.Equal(sql.NullString{String: "synthetic-app", Valid: true}, app)
			}
		})
	}
}
