package store

import (
	"reflect"
	"testing"
)

// TestSplitSchemaStatements: the splitter is lexically aware —
// semicolons inside literals, quoted identifiers, comments, and
// dollar-quoted bodies do not end a statement, and comments are
// stripped. The old splitter cut on every ";" outside full-line
// comments.
func TestSplitSchemaStatements(t *testing.T) {
	script := `-- a full-line comment; with a semicolon
CREATE TABLE split_test (
	id bigint PRIMARY KEY, -- trailing comment; stripped
	note text DEFAULT 'it''s; quoted',
	"we;ird" text
); /* a block; comment */ INSERT INTO split_test VALUES (1, 'a;b', 'c');
CREATE FUNCTION split_fn() RETURNS void AS $body$
BEGIN
	RAISE NOTICE 'done;';
END;
$body$ LANGUAGE plpgsql;
SELECT $1, E'esca\'ped;';`
	got := splitSchemaStatements(script)
	want := []string{
		"CREATE TABLE split_test (\n\tid bigint PRIMARY KEY, \n\tnote text DEFAULT 'it''s; quoted',\n\t\"we;ird\" text\n)",
		"INSERT INTO split_test VALUES (1, 'a;b', 'c')",
		"CREATE FUNCTION split_fn() RETURNS void AS $body$\nBEGIN\n\tRAISE NOTICE 'done;';\nEND;\n$body$ LANGUAGE plpgsql",
		"SELECT $1, E'esca\\'ped;'",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitSchemaStatements =\n%q\nwant\n%q", got, want)
	}
}

// TestSplitSchemaStatementsDollarTags: a $1 parameter is not a
// quote, and nested block comments close in order.
func TestSplitSchemaStatementsDollarTags(t *testing.T) {
	script := "SELECT $1, $$a;b$$, $tag$x;y$tag$; /* outer /* inner ; */ still ; */ SELECT 2;"
	got := splitSchemaStatements(script)
	want := []string{"SELECT $1, $$a;b$$, $tag$x;y$tag$", "SELECT 2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitSchemaStatements = %q, want %q", got, want)
	}
}
