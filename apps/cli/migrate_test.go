package main

import (
	"testing"

	"dedie/engine/migrate"
)

func TestLeftoverScript(t *testing.T) {
	got := leftoverScript([]migrate.Leftover{
		{Kind: "view", Name: "shop.ranked", Reason: "it calls RANK()", Source: "CREATE VIEW ranked AS SELECT 1;\n"},
		{Kind: "procedure", Name: "SHOP.RESTOCK", Reason: "PL/SQL is not translated", Source: "CREATE OR REPLACE PROCEDURE restock IS\nBEGIN\n  NULL;\nEND;", End: "/"},
		{Kind: "function", Name: "shop.hidden", Reason: "not translated; the source account may not read its definition"},
	})
	want := `-- What this migration does not carry over, as the source defines it.
-- This is the source's dialect: convert it before running it on the target.

-- view shop.ranked
-- Not migrated: it calls RANK()
CREATE VIEW ranked AS SELECT 1;

-- procedure SHOP.RESTOCK
-- Not migrated: PL/SQL is not translated
CREATE OR REPLACE PROCEDURE restock IS
BEGIN
  NULL;
END;
/

-- function shop.hidden
-- Not migrated: not translated; the source account may not read its definition
-- (the source did not show its definition)

`
	if got != want {
		t.Errorf("script:\n%s\nwant:\n%s", got, want)
	}
}
