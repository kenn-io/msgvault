package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureReadOnly(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		allowed bool
	}{
		// Plain read-only statements.
		{"simple select", "SELECT 1", true},
		{"select from view", "SELECT * FROM v_senders", true},
		{"lowercase select", "select count(*) from messages", true},
		{"leading whitespace", "   \n\t SELECT 1", true},
		{"trailing semicolon", "SELECT 1;", true},
		{"trailing semicolon and space", "SELECT 1;   ", true},
		{"parenthesized select", "(SELECT 1) UNION (SELECT 2)", true},
		{"from-first shorthand", "FROM messages SELECT count(*)", true},
		{"from-first only", "FROM messages", true},
		{"table shorthand", "TABLE messages", true},
		{"values", "VALUES (1), (2), (3)", true},
		{"describe", "DESCRIBE messages", true},
		{"desc", "DESC messages", true},
		{"show tables", "SHOW TABLES", true},
		{"summarize", "SUMMARIZE messages", true},
		{"pivot", "PIVOT messages ON message_type USING count(*)", true},

		// Comments.
		{"line comment before", "-- a comment\nSELECT 1", true},
		{"block comment before", "/* comment */ SELECT 1", true},
		{"comment only", "-- just a comment", false},
		{"block comment only", "/* nothing here */", false},
		{"line comment hides drop", "SELECT 1 -- ; DROP TABLE x", true},
		{"block comment hides drop", "SELECT 1 /* ; DROP TABLE x */", true},

		// CTEs — classify the statement the CTE feeds, not the WITH prefix.
		{"with select", "WITH t AS (SELECT 1) SELECT * FROM t", true},
		{"with recursive select", "WITH RECURSIVE t AS (SELECT 1) SELECT * FROM t", true},
		{"with two ctes", "WITH a AS (SELECT 1), b AS (SELECT 2) SELECT * FROM a, b", true},
		{"with materialized", "WITH t AS MATERIALIZED (SELECT 1) SELECT * FROM t", true},
		{"with delete", "WITH t AS (SELECT id FROM messages) DELETE FROM messages WHERE id IN (SELECT id FROM t)", false},
		{"with insert", "WITH t AS (SELECT 1 AS n) INSERT INTO messages SELECT n FROM t", false},
		{"with update", "WITH t AS (SELECT 1) UPDATE messages SET subject = 'x'", false},

		// EXPLAIN wraps another statement.
		{"explain select", "EXPLAIN SELECT 1", true},
		{"explain with", "EXPLAIN WITH t AS (SELECT 1) SELECT * FROM t", true},
		{"explain analyze select rejected", "EXPLAIN ANALYZE SELECT 1", false},
		{"explain delete rejected", "EXPLAIN DELETE FROM messages", false},

		// Semicolons and keywords inside string literals must not matter.
		{"semicolon in string literal", "SELECT ';'", true},
		{"drop word in string literal", "SELECT ';DROP'", true},
		{"drop table text in string", "SELECT 'DROP TABLE messages' AS note", true},
		{"escaped quote in string", "SELECT 'it''s fine; really'", true},
		{"keyword in quoted identifier", `SELECT 1 AS "delete"`, true},
		{"semicolon in dollar quote", "SELECT $$a;b$$", true},
		{"tagged dollar quote", "SELECT $tag$ DROP TABLE x; $tag$", true},

		// Multi-statement input is rejected regardless of statement kind.
		{"two selects", "SELECT 1; SELECT 2", false},
		{"select then drop", "SELECT 1; DROP TABLE messages", false},
		{"select then delete", "SELECT * FROM messages; DELETE FROM messages", false},
		{"drop then select", "DROP TABLE messages; SELECT 1", false},

		// Direct writes / DDL / session mutations.
		{"delete", "DELETE FROM messages", false},
		{"delete with where", "DELETE FROM messages WHERE id = 1", false},
		{"insert", "INSERT INTO messages (id) VALUES (1)", false},
		{"update", "UPDATE messages SET subject = 'x' WHERE id = 1", false},
		{"drop table", "DROP TABLE messages", false},
		{"create table", "CREATE TABLE t (id INT)", false},
		{"create table as select", "CREATE TABLE t AS SELECT * FROM messages", false},
		{"create view", "CREATE VIEW v AS SELECT 1", false},
		{"alter table", "ALTER TABLE messages ADD COLUMN x INT", false},
		{"truncate", "TRUNCATE messages", false},
		{"attach", "ATTACH 'evil.db' AS evil", false},
		{"detach", "DETACH sqlite_db", false},
		{"copy to file", "COPY (SELECT 1) TO 'out.csv'", false},
		{"copy from file", "COPY messages FROM 'in.csv'", false},
		{"install extension", "INSTALL httpfs", false},
		{"load extension", "LOAD httpfs", false},
		{"set session", "SET threads = 1", false},
		{"pragma", "PRAGMA database_list", false},
		{"call", "CALL pragma_version()", false},
		{"empty", "", false},
		{"whitespace only", "   \n  ", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := EnsureReadOnly(tc.sql)
			if tc.allowed {
				assert.NoError(t, err, "expected %q to be allowed", tc.sql)
				return
			}
			require.Error(t, err, "expected %q to be rejected", tc.sql)
			assert.ErrorIs(t, err, ErrQueryNotReadOnly,
				"rejection should wrap ErrQueryNotReadOnly")
		})
	}
}
