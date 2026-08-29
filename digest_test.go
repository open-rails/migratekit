package migratekit

import (
	"strings"
	"testing"
)

func TestSemanticContentDigestIgnoresFormatting(t *testing.T) {
	base := `CREATE TABLE Example (id bigint PRIMARY KEY, note text);
INSERT INTO Example VALUES (1, 'a -- value');`
	formatted := `
-- explanation only
create /* nested /* comment */ still comment */ table example(
  id BIGINT primary key,
  note TEXT
);
insert into example values(1,'a -- value');
`
	if SemanticContentDigest(base) != SemanticContentDigest(formatted) {
		t.Fatal("comments, whitespace, and unquoted identifier case changed the semantic digest")
	}
	changed := strings.Replace(formatted, "BIGINT", "INTEGER", 1)
	if SemanticContentDigest(base) == SemanticContentDigest(changed) {
		t.Fatal("a DDL token change did not change the semantic digest")
	}
}

func TestSemanticContentDigestPreservesQuotedBodies(t *testing.T) {
	base := `CREATE FUNCTION f() RETURNS text AS $$ BEGIN RETURN 'a'; END $$ LANGUAGE plpgsql;`
	changed := `CREATE FUNCTION f() RETURNS text AS $$ BEGIN RETURN 'b'; END $$ LANGUAGE plpgsql;`
	if SemanticContentDigest(base) == SemanticContentDigest(changed) {
		t.Fatal("dollar-quoted executable content did not change the semantic digest")
	}
	quoted := `SELECT "CaseSensitive", E'a\\'b';`
	if SemanticContentDigest(quoted) == SemanticContentDigest(`SELECT "casesensitive", E'a\\'b';`) {
		t.Fatal("quoted identifier case did not change the semantic digest")
	}
}
