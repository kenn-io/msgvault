package store_test

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestInitSchemaUnlessCurrentPostgreSQLRepairs(t *testing.T) {
	for _, repair := range []string{"default collection", "account identity keys", "system seeds"} {
		t.Run(repair, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := storetest.New(t)
			if !f.Store.IsPostgreSQL() {
				t.Skip("requires PostgreSQL")
			}
			ctx := t.Context()
			switch repair {
			case "default collection":
				_, err := f.Store.DB().ExecContext(ctx, `DELETE FROM collection_sources WHERE source_id = $1`, f.Source.ID)
				require.NoError(err)

				require.NoError(f.Store.InitSchemaUnlessCurrentContext(ctx))
				collection, err := f.Store.GetCollectionByNameContext(ctx, store.DefaultCollectionName)
				require.NoError(err)
				assert.Equal([]int64{f.Source.ID}, collection.SourceIDs)
			case "account identity keys":
				require.NoError(f.Store.AddAccountIdentityContext(ctx, f.Source.ID, "Alice@Example.com", "manual"))
				// A previous-release writer omits address_key, leaving its default ''.
				_, err := f.Store.DB().ExecContext(ctx,
					`INSERT INTO account_identities (source_id, address, source_signal) VALUES ($1, 'alice@example.com', 'header')`, f.Source.ID)
				require.NoError(err)

				require.NoError(f.Store.InitSchemaUnlessCurrentContext(ctx))
				identities, err := f.Store.ListAccountIdentitiesContext(ctx, f.Source.ID)
				require.NoError(err)
				assert.Len(identities, 1, "current archives repair case variants left by previous-release writers")
			case "system seeds":
				definition, err := f.Store.GetAttributeDefinitionBySlugContext(ctx, store.AttributeObjectPerson, store.AttributeSlugAskMeAbout)
				require.NoError(err)
				_, err = f.Store.DB().ExecContext(ctx,
					`UPDATE attribute_definitions SET options = '{"max_length":120}' WHERE universal_id = $1`, definition.UniversalID)
				require.NoError(err)
				_, err = f.Store.DB().ExecContext(ctx, `UPDATE relationship_types SET is_symmetric = FALSE WHERE slug = 'neighbor'`)
				require.NoError(err)

				require.NoError(f.Store.InitSchemaUnlessCurrentContext(ctx))
				definition, err = f.Store.GetAttributeDefinitionBySlugContext(ctx, store.AttributeObjectPerson, store.AttributeSlugAskMeAbout)
				require.NoError(err)
				require.NotNil(definition.Options)
				assert.Equal(280, definition.Options.MaxLength)
				repaired, err := f.Store.GetRelationshipTypeBySlugContext(ctx, "neighbor")
				require.NoError(err)
				assert.True(repaired.IsSymmetric)
			}
		})
	}
}

func TestInitSchemaUnlessCurrentPostgreSQLFallsThroughToFullInitializer(t *testing.T) {
	for _, version := range []int{store.SchemaVersion - 1, store.SchemaVersion + 1} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			require := require.New(t)
			f := storetest.New(t)
			if !f.Store.IsPostgreSQL() {
				t.Skip("requires PostgreSQL")
			}
			ctx := t.Context()
			if version < store.SchemaVersion {
				_, err := f.Store.DB().ExecContext(ctx, `DROP INDEX idx_identity_match_candidates_right`)
				require.NoError(err)
			}
			_, err := f.Store.DB().ExecContext(ctx, `UPDATE archive_metadata SET value = $1 WHERE key = 'schema_version'`, strconv.Itoa(version))
			require.NoError(err)

			err = f.Store.InitSchemaUnlessCurrentContext(ctx)
			if version > store.SchemaVersion {
				require.ErrorContains(err, "newer")
				got, err := f.Store.SchemaVersionContext(ctx)
				require.NoError(err)
				assert.Equal(t, version, got)
				return
			}
			require.NoError(err)
			got, err := f.Store.SchemaVersionContext(ctx)
			require.NoError(err)
			assert.Equal(t, store.SchemaVersion, got)
			var indexes int
			require.NoError(f.Store.DB().QueryRowContext(ctx,
				`SELECT count(*) FROM pg_indexes WHERE schemaname = current_schema() AND indexname = 'idx_identity_match_candidates_right'`).Scan(&indexes))
			assert.Equal(t, 1, indexes, "upgrading an archive recreates missing schema objects")
		})
	}
}

func TestSchemaVersionCandidateRightIndexUpgrade(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL index check")
	}
	_, err := f.Store.DB().ExecContext(t.Context(), "DROP INDEX idx_identity_match_candidates_right")
	require.NoError(err)
	require.NoError(f.Store.InitSchemaContext(t.Context()))
	require.NoError(f.Store.ValidateSchemaContext(t.Context()))
	var count int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM pg_indexes WHERE schemaname = current_schema() AND indexname = 'idx_identity_match_candidates_right'`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestSchemaVersionDocbankUpgrade(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	_, err := f.Store.DB().ExecContext(t.Context(), "DROP TABLE docbank_attachment_deliveries")
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), "UPDATE archive_metadata SET value='9' WHERE key='schema_version'")
	require.NoError(err)
	require.Error(f.Store.ValidateSchemaContext(t.Context()))
	require.NoError(f.Store.InitSchemaContext(t.Context()))
	version, err := f.Store.SchemaVersionContext(t.Context())
	require.NoError(err)
	assert.Equal(store.SchemaVersion, version)
	var count int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM docbank_attachment_deliveries").Scan(&count))
	assert.Zero(count)
}

func TestSchemaVersionSQLiteFailedUpgrade(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s, err := store.OpenForTest(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(s.Close()) })
	_, err = s.DB().ExecContext(t.Context(), "CREATE TABLE archive_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL); CREATE TABLE messages (wrong_column TEXT)")
	require.NoError(err)
	require.Error(s.InitSchemaContext(t.Context()))
	var markers int
	require.NoError(s.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM archive_metadata WHERE key='schema_version'").Scan(&markers))
	assert.Zero(markers, "failed upgrade must not certify completion")
}

func TestSchemaVersionSQLiteFutureArchive(t *testing.T) {
	for _, version := range []int64{store.SchemaVersion + 1, math.MaxInt32} {
		t.Run(strconv.FormatInt(version, 10), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			s, err := store.OpenForTest(t.Context(), filepath.Join(t.TempDir(), "future.db"))
			require.NoError(err)
			t.Cleanup(func() { require.NoError(s.Close()) })
			_, err = s.DB().ExecContext(t.Context(), "CREATE TABLE archive_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)")
			require.NoError(err)
			_, err = s.DB().ExecContext(t.Context(), "INSERT INTO archive_metadata (key, value) VALUES ('schema_version', ?)", version)
			require.NoError(err)
			_, err = s.DB().ExecContext(t.Context(), "CREATE TABLE sentinel (value TEXT); INSERT INTO sentinel VALUES ('keep')")
			require.NoError(err)
			require.ErrorContains(s.InitSchemaContext(t.Context()), "newer")
			var got int64
			require.NoError(s.DB().QueryRowContext(t.Context(), "SELECT value FROM archive_metadata WHERE key='schema_version'").Scan(&got))
			assert.Equal(version, got)
			var value string
			require.NoError(s.DB().QueryRowContext(t.Context(), "SELECT value FROM sentinel").Scan(&value))
			assert.Equal("keep", value)
			var tables int
			require.NoError(s.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='messages'").Scan(&tables))
			assert.Zero(tables, "future archive must be refused before schema DDL")
		})
	}
}

func TestSchemaVersionLegacyMarker(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s := testutil.NewTestStore(t)
	ctx := t.Context()
	version, err := s.SchemaVersionContext(ctx)
	require.NoError(err)
	assert.Equal(store.SchemaVersion, version)
	require.NoError(s.InitSchemaContext(ctx))
	version, err = s.SchemaVersionContext(ctx)
	require.NoError(err)
	assert.Equal(store.SchemaVersion, version)
	_, err = s.DB().ExecContext(t.Context(), "DELETE FROM archive_metadata WHERE key='schema_version'")
	require.NoError(err)
	version, err = s.SchemaVersionContext(ctx)
	require.NoError(err)
	assert.Zero(version)
	require.NoError(s.InitSchemaContext(ctx))
	version, err = s.SchemaVersionContext(ctx)
	require.NoError(err)
	assert.Equal(store.SchemaVersion, version)
}

func TestSchemaVersionInlineServiceUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		version              int
		missingInline        bool
		missingRouteFailures bool
	}{
		{name: "legacy6", version: 6, missingInline: true, missingRouteFailures: true},
		{name: "upstream11", version: 11, missingInline: true},
		{name: "inline7", version: 7, missingRouteFailures: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			s := testutil.NewTestStore(t)
			ctx := t.Context()
			uid, err := s.ArchiveUIDContext(ctx)
			require.NoError(err)

			// Archives missing the Inline seed or the route-failure table
			// need setup before runtime readers can open them.
			if tc.missingInline {
				_, err = s.DB().ExecContext(ctx, `DELETE FROM applied_migrations WHERE name = 'communication_services_inline_v1'`)
				require.NoError(err)
				_, err = s.DB().ExecContext(ctx, `DELETE FROM communication_services WHERE slug = 'inline'`)
				require.NoError(err)
			}
			if tc.missingRouteFailures {
				_, err = s.DB().ExecContext(ctx, `DROP TABLE source_messaging_route_failures`)
				require.NoError(err)
			}
			_, err = s.DB().ExecContext(ctx, s.Rebind(`UPDATE archive_metadata SET value = ? WHERE key = 'schema_version'`), strconv.Itoa(tc.version))
			require.NoError(err)

			require.ErrorContains(s.ValidateSchemaContext(ctx), "run setup")
			version, err := s.SchemaVersionContext(ctx)
			require.NoError(err)
			assert.Equal(tc.version, version, "read-only validation must leave the old completion marker alone")
			if tc.missingInline {
				_, err = s.ResolveCommunicationServiceContext(ctx, "inline")
				require.ErrorIs(err, store.ErrServiceNotFound, "read-only validation must not seed the missing service")
			}
			if tc.missingRouteFailures {
				var rows int
				err = s.DB().QueryRowContext(ctx, `SELECT count(*) FROM source_messaging_route_failures`).Scan(&rows)
				require.Error(err, "read-only validation must not create the missing route-failure table")
			}

			require.NoError(s.InitSchemaContext(ctx))
			require.NoError(s.ValidateSchemaContext(ctx))
			version, err = s.SchemaVersionContext(ctx)
			require.NoError(err)
			assert.Equal(store.SchemaVersion, version)
			service, err := s.ResolveCommunicationServiceContext(ctx, "inline")
			require.NoError(err)
			assert.True(service.IsSystem)
			assert.Equal(store.ScopePolicyRequired, service.ScopePolicy)
			var applied int
			require.NoError(s.DB().QueryRowContext(ctx,
				`SELECT count(*) FROM applied_migrations WHERE name = 'communication_services_inline_v1'`,
			).Scan(&applied))
			assert.Equal(1, applied)
			var failures int
			require.NoError(s.DB().QueryRowContext(ctx, `SELECT count(*) FROM source_messaging_route_failures`).Scan(&failures))
			assert.Zero(failures)
			again, err := s.ArchiveUIDContext(ctx)
			require.NoError(err)
			assert.Equal(uid, again, "setup must preserve the archive identity")
		})
	}
}

func TestSchemaVersionMalformedAndFuture(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	s := testutil.NewTestStore(t)
	for _, value := range []string{"bad", "0", "-1", "999999999999999999999999999999999", strconv.Itoa(store.SchemaVersion + 1)} {
		t.Run(value, func(t *testing.T) {
			req := require.New(t)
			check := assert.New(t)
			_, err := s.DB().ExecContext(t.Context(), s.Rebind("UPDATE archive_metadata SET value = ? WHERE key='schema_version'"), value)
			req.NoError(err)
			req.Error(s.InitSchemaContext(t.Context()))
			var got string
			req.NoError(s.DB().QueryRowContext(t.Context(), "SELECT value FROM archive_metadata WHERE key='schema_version'").Scan(&got))
			check.Equal(value, got)
		})
	}
	_, err := s.DB().ExecContext(t.Context(), "DROP TABLE archive_metadata")
	req.NoError(err)
	version, err := s.SchemaVersionContext(t.Context())
	req.NoError(err)
	check.Zero(version, "missing legacy metadata must not be created by probe")
}

// schemaContractDigests and schemaContractPostgresDigests record, per
// SchemaVersion, the digest of what that version builds on each backend.
// Append only.
var (
	schemaContractDigests = []string{
		1:  "08c288dc75ccc5c75f4513cf92a10f34a75b5e918f7ceea83bf870040e252358",
		2:  "ad86bbf0db74619be9eaefae6f34bcaad37f3520a9a302810c0fc867735cd4c1",
		3:  "bde397bba4bf884c55327b3956af11cd69400f7fdfdf60b34a05eaeeb5534b84",
		4:  "ed0092e4f507e327350bb9bbf2f0cfcd3cb8e53c9ca57f509c514e7e3f68579e",
		5:  "ed0092e4f507e327350bb9bbf2f0cfcd3cb8e53c9ca57f509c514e7e3f68579e",
		6:  "c4e06b60819045ffe63afda9caeb916e2b5f9d3b77f7ffc7c06b037625a8a2b7",
		7:  "4556a30227e7e696a688d1e96d71c565304c6119e6964d8933085c47a42dad34",
		8:  "9580fe5b37f693d1711c74b31f6651268c99a6016039fcf023821502853793fd",
		9:  "c07287fd6b88c73e3e34d8a2b23ed064916d4ea290bbe416561860e992f4ec53",
		10: "da0cc2d980a72019a4b62685f0938c23248237b5dad6e98e6e58756cf0a445aa",
		11: "d3b5779bbd283bb4a6ccc932233bdbf7b4ea4d6c2453d43cf011a052a13c1064",
		12: "77b6d3434f3091970112c2a27f20f8b9e940c0a71b5a16b280d552b3857412b5",
		13: "c5ccb9e5b3659b94a2d66acfd0cf8ea323e70abb3000f3c5792c10ecf79f85b8",
		14: "1092127cfc654d884f52634204dee6d04d5c4c9d966fe997263d064c871a21ee",
		15: "571660ef3e1c045c57ada148b252d90acd7ef291458a7a46a1327b1034dde653",
		16: "62de77a40a4f77177c2bab8d38ee3df0c1c269ca2818c11ae95ad3f0a7e23656",
		17: "b8cd27ce189a5c7be16d8bef6208a46e622cf796362eb40ba19043fd5fa08bdf",
		18: "aa3901c4c46a3dbf2575830f2ff6fc16f2f9f66ce8e2c395c8657663db56b8f0",
		19: "5cf89cccb1daee63eb6ed983101263511b6f06990c4c4677e0ecc186166bf209",
		24: "743ff4c80a91bd368adb9c140b5f099cef3b604e97a32a4d1a7a7356f77efb48",
		25: "b47d58c3e09db075a9e66df4c1544e406781e5944e6ec8426e7eb5f95549e51f",
		27: "0fa85360e177bed150718440d3aea27b8d0eb71b70859608239ddafa26526abd",
		28: "fa82fa81b7c53e7043ceb4d98855155372ae81d7dbb916c6e4e13957543e6f23",
		29: "d99ea22b46701498162c909cb7a0f2404f6487e42e0ba0bcf8769c58d0814194",
		30: "41d1794a318cda45f41e498843ee7f7bff6d96ab206b4ed9dfc4d52dc52ea270",
		31: "cad2b896394a3b1df1cb8b71cc2db52e893623272d87e1ede64ad757e6aba500",
		32: "67195ce591cb53fde19ccf89d46f57a8d1cba7fcb96efd0b1d120dcde4c0dceb",
		33: "7158a0bdc0768658194eacd650486e790f43ce0ab26f9aeffcd01843e3958ecb",
		34: "46790542c505c944c29e50066f1875ac7cfe3ea42cccf0b0ec098ff23ca5d1eb",
		35: "20e21e50260dd31c150213d13e0600661424e54885c9938e6895314dde23a2aa",
		36: "ad076c4da9652ccf15eb7cb0a55cf57d6ec49f8c5db885db0bc44cac836c79ce",
		37: "f6d6ae3d753476a4261d54df5d413994ec987dc34137fccf421f215bf0a11b7e",
		38: "ac18227f57322ed8e822c5406000375e7e3aa2b47c73246ba36e9b0628c9bb66",
		39: "7be9b19aa60a7ccec120d67fcab872131baf328f877443ab5bfe47a454400b37",
	}
	schemaContractPostgresDigests = []string{
		1:  "7c554ff06468af9bf30950e6e4f2954b52f84d7dd0e6708f4fdd932fda007ed3",
		2:  "7f207ef54527a376aebc79ee9c2217e3af0bfb1171ed98af2ef5504ea7dec7ca",
		3:  "a60a2d5f70be37cbfd6ea4bf8c264be5d70f1dec16dd87d611ead7c0b01ec823",
		4:  "68f409716bd517ddda67402e966ff5d8fb2d00326ffef42fedccb77a0f11584c",
		5:  "68f409716bd517ddda67402e966ff5d8fb2d00326ffef42fedccb77a0f11584c",
		6:  "9a63d5ae5ad40ebca9500de9b3de10cdbce185d7ca8f0039e49db3b22e2f2fc0",
		7:  "dd594306d490f5788e087fc49fb223370d2d5cf6d0d457666770d45adf59b7d5",
		8:  "eeda778f8f171d2f2eb0b1d9dd00d725a40a7c05832738f1fb295f57bb892f1d",
		9:  "3291ad98ccf60a283854288956d5784dc2c1ce122f521aa8123b200ae16791c6",
		10: "abe822a2b3ba31bca556d4da218fd6542b3e4ba95acf4731231f47fc6a2a44f8",
		11: "0a64c9d206531b241d1066d3426dd21f6334d1e2c99468d838b8dac4a794cf70",
		12: "21a6d84fac2dd2c3c1d04d23e56e6fc977dfb4431a11beed75678274fbdf11b3",
		13: "da8aa0a2023db284656a471e2640b80ec2be2a5d110136cb8e62f81739a384ac",
		14: "00ca50153fc0f90dc588225b0ce9b9097caf71dc42a47e73e7d03f2bfc460a93",
		15: "47f70765d8793d9fdbbe65b259829caffb39d737d386216820f3067d4771c244",
		16: "ccfde395fe0b24f87e1a32375c67eab640f768083de3e3e5ef965ab918b08903",
		17: "08e0d79f0aced4f968ff10240fbd16b8af90bab114e8ac15e2dde7500c8eed86",
		18: "0b50a4dde53b4c696b678b26f07bf34d09809231d76c264603a00fc1865b9f07",
		19: "bc49846bd633ccbd5930a60cd90fdc50ce392b354b1789936df543306efa36a1",
		24: "ba816feeaa25dde2fdc93221f47c84bf283e2798c4b92cec1534cd087b3df552",
		25: "b8d1c951733d7fd80dcac39ed8bb38fccd11e622712c8f7b108b2f16dc2f3d3f",
		27: "21dcda6fd8282b00a610c195484955ad74cf5d82745e3cfd70ebae94c1882c31",
		28: "8635b895db8e63c0047919852ebd9d06969867b5ee9dbe0d5864775ee39b1357",
		29: "9b5dde3f88bebb178ad918e86607113f312fdda56bd859b72450286e1a1f6182",
		30: "9b5dde3f88bebb178ad918e86607113f312fdda56bd859b72450286e1a1f6182",
		31: "f4b83986f7def2d4f1f49dce1074b828bb0bb2a3ff042ced1a512b8443407c90",
		32: "d1947235dc5ffd4d0c8f611722e887faa3464bfaa8565e8713af1701e128b947",
		33: "d1947235dc5ffd4d0c8f611722e887faa3464bfaa8565e8713af1701e128b947",
		34: "7a1753c30acf49ee4b97ec23e42228cd94f70b0f3dca308bffd0f038d5461f59",
		35: "7a1753c30acf49ee4b97ec23e42228cd94f70b0f3dca308bffd0f038d5461f59",
		36: "9bd56e75e273ac9afb7f9acdd76732b2494d011b57f88951951d9be1649824d1",
		37: "7620e8c5b65db28a3b5bd6a0f709e53df28c5d7ee2fae42454e372508d9bc10e",
		38: "5244b660c5b79ef46144ba5a9fa9470f84a15fe6b2cb1a3ffd11d2a39157ec32",
		39: "2c48399a1a968a87a127d7d42991f166df075b5a2c50b787a7dfe703a4aa9af6",
	}
)

func TestSchemaVersionContract(t *testing.T) {
	checkSchemaContract(t, "schemaContractDigests", schemaContractDigests, schemaContractDigest(t))
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		s := testutil.NewTestStore(t)
		checkSchemaContract(t, "schemaContractPostgresDigests", schemaContractPostgresDigests, schemaContractPostgresDigest(t, s))
	} else if len(schemaContractPostgresDigests) != len(schemaContractDigests) {
		assert.Failf(t, "missing PostgreSQL schema digest", "append the PostgreSQL digest for SchemaVersion %d to schemaContractPostgresDigests; "+
			"this test prints it when run with MSGVAULT_TEST_DB set to a PostgreSQL URL", store.SchemaVersion)
	}
}

func checkSchemaContract(t *testing.T, list string, digests []string, digest string) {
	t.Helper()
	last := len(digests) - 1
	if last != store.SchemaVersion || digests[last] != digest {
		assert.Failf(t, "SchemaVersion not bumped", "schema or migration list changed without a SchemaVersion bump: "+
			"set store.SchemaVersion to %d and append %q to %s", max(last+1, store.SchemaVersion), digest, list)
	}
}

// schemaContractPostgresDigest hashes the tables, columns, constraints,
// indexes, views, triggers and functions a fresh PostgreSQL archive gets, plus
// its migration ledger. Schema names are stripped because each test runs in
// its own database or schema; extension-owned objects are skipped.
func schemaContractPostgresDigest(t *testing.T, s *store.Store) string {
	t.Helper()
	parts := queryStrings(t, s, `
WITH ns AS (SELECT oid, nspname FROM pg_namespace WHERE nspname = current_schema()),
own AS (SELECT c.oid, c.relname, c.relkind FROM pg_class c JOIN ns ON c.relnamespace = ns.oid
	WHERE NOT EXISTS (SELECT 1 FROM pg_depend d
		WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e'))
SELECT replace(def, (SELECT nspname FROM ns) || '.', '') FROM (
	SELECT 'column ' || o.relname || '.' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod) ||
		CASE WHEN a.attnotnull THEN ' not null' ELSE '' END ||
		CASE WHEN a.attidentity <> '' THEN ' identity ' || a.attidentity::text ELSE '' END ||
		coalesce(' collate ' || coll.collname, '') ||
		coalesce(' default ' || pg_get_expr(ad.adbin, ad.adrelid), '') ||
		CASE WHEN a.attgenerated <> '' THEN ' generated' ELSE '' END AS def
	FROM own o JOIN pg_attribute a ON a.attrelid = o.oid
	LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
	LEFT JOIN pg_type ty ON ty.oid = a.atttypid
	LEFT JOIN pg_collation coll ON coll.oid = a.attcollation AND a.attcollation <> ty.typcollation
	WHERE o.relkind IN ('r', 'p', 'v', 'm') AND a.attnum > 0 AND NOT a.attisdropped
	UNION ALL
	SELECT 'constraint ' || o.relname || '.' || con.conname || ' ' || pg_get_constraintdef(con.oid)
	FROM own o JOIN pg_constraint con ON con.conrelid = o.oid
	-- PostgreSQL 18 lists NOT NULL as constraints; attnotnull already covers it.
	WHERE con.contype <> 'n'
	UNION ALL
	SELECT 'index ' || pg_get_indexdef(o.oid) FROM own o WHERE o.relkind = 'i'
	UNION ALL
	SELECT 'sequence ' || o.relname || ' ' || format_type(sq.seqtypid, NULL) || ' ' || sq.seqstart || ' ' ||
		sq.seqincrement || ' ' || sq.seqmin || ' ' || sq.seqmax || ' ' || sq.seqcache || ' ' || sq.seqcycle
	FROM own o JOIN pg_sequence sq ON sq.seqrelid = o.oid
	UNION ALL
	SELECT 'view ' || o.relname || ' ' || pg_get_viewdef(o.oid) FROM own o WHERE o.relkind IN ('v', 'm')
	UNION ALL
	SELECT 'trigger ' || pg_get_triggerdef(tg.oid) FROM own o JOIN pg_trigger tg ON tg.tgrelid = o.oid
	WHERE NOT tg.tgisinternal
	UNION ALL
	SELECT 'function ' || pg_get_functiondef(p.oid) FROM pg_proc p JOIN ns ON p.pronamespace = ns.oid
	WHERE p.prokind IN ('f', 'p')
		AND NOT EXISTS (SELECT 1 FROM pg_depend d
			WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
) defs ORDER BY 1`)
	for i, part := range parts {
		parts[i] = normalizeSQL(part)
	}
	parts = append(parts, queryStrings(t, s, `SELECT name || ' ' || version FROM applied_migrations ORDER BY name`)...)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

// schemaContractDigest hashes what InitSchemaContext builds on a fresh SQLite
// archive (schema objects and migration ledger rows) plus both dialects'
// legacy column statements and the FTS and PostgreSQL schema files. FTS
// objects exist only under the fts5 build tag, so they come from
// schema_sqlite.sql instead.
func schemaContractDigest(t *testing.T) string {
	t.Helper()
	s, err := store.OpenForTest(t.Context(), filepath.Join(t.TempDir(), "contract.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.InitSchemaContext(t.Context()))
	var parts []string
	for _, object := range queryStrings(t, s, `SELECT type || ' ' || name || ' ' || sql FROM sqlite_master
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite!_%' ESCAPE '!' AND name NOT LIKE '%!_fts%' ESCAPE '!'
		ORDER BY type, name`) {
		parts = append(parts, normalizeSQL(object))
	}
	parts = append(parts, queryStrings(t, s, `SELECT name || ' ' || version FROM applied_migrations ORDER BY name`)...)
	// Legacy ADD COLUMN statements only run on old archives, so a fresh one
	// never shows them.
	for _, d := range []store.Dialect{&store.SQLiteDialect{}, &store.PostgreSQLDialect{}} {
		for _, m := range d.LegacyColumnMigrations() {
			parts = append(parts, normalizeSQL(m.SQL))
		}
	}
	for _, name := range []string{"schema_sqlite.sql", "schema_pg.sql"} {
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		parts = append(parts, normalizeSQL(string(data)))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

func queryStrings(t *testing.T, s *store.Store, query string) []string {
	t.Helper()
	rows, err := s.DB().QueryContext(t.Context(), query)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var out []string
	for rows.Next() {
		var value string
		require.NoError(t, rows.Scan(&value))
		out = append(out, value)
	}
	require.NoError(t, rows.Err())
	return out
}

// normalizeSQL drops comments and collapses whitespace outside quoted text, so
// comment edits and CRLF checkouts keep the digest while literals stay exact.
func normalizeSQL(sql string) string {
	var out strings.Builder
	var quote byte
	space := false
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			space = true
			continue
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			space = true
			continue
		}
		if space && out.Len() > 0 {
			out.WriteByte(' ')
		}
		space = false
		out.WriteByte(c)
	}
	return out.String()
}

func TestNormalizeSQL(t *testing.T) {
	assert.Equal(t, "CREATE TABLE t ( a TEXT DEFAULT ' -- x  ' );", normalizeSQL("-- note\r\nCREATE  TABLE t (\r\n  a TEXT DEFAULT ' -- x  ' -- trailing\r\n);\r\n"))
	assert.NotEqual(t, normalizeSQL("SELECT ' '"), normalizeSQL("SELECT '  '"))
}
