package main

import (
	"testing"

	"dedie/engine/migrate"
)

func TestLeftoverScript(t *testing.T) {
	got := leftoverScript([]migrate.Leftover{
		{Kind: "view", Name: "shop.ranked", Reason: "it calls RANK()", Source: "CREATE VIEW ranked AS SELECT 1;\n"},
		{Kind: "procedure", Name: "SHOP.RESTOCK", Reason: "packages are not converted", Status: "unconverted", Source: "CREATE OR REPLACE PROCEDURE restock IS\nBEGIN\n  NULL;\nEND;", End: "/"},
		{Kind: "function", Name: "SHOP.RATIO", Reason: "it was converted, but 1 place(s) may behave differently", Status: "review",
			Notes: []string{"line 3: the division is not rounded"}, Source: "CREATE OR REPLACE FUNCTION ratio RETURN NUMBER IS\nBEGIN\n  RETURN 1 / 3;\nEND;", End: "/",
			Draft: "CREATE FUNCTION shop.ratio() RETURNS numeric\nLANGUAGE plpgsql\nAS $body$\nBEGIN\n  RETURN 1 / 3::numeric;\nEND;\n$body$;"},
		{Kind: "function", Name: "shop.hidden", Reason: "the source account may not read its definition"},
	})
	want := `-- What this migration does not carry over, or carried over with a caveat.
-- A converted draft is in the target's dialect; everything else is as the
-- source defines it, and has to be converted before it runs on the target.

-- view shop.ranked
-- it calls RANK()
CREATE VIEW ranked AS SELECT 1;

-- procedure SHOP.RESTOCK  [unconverted]
-- packages are not converted
CREATE OR REPLACE PROCEDURE restock IS
BEGIN
  NULL;
END;
/

-- function SHOP.RATIO  [review]
-- it was converted, but 1 place(s) may behave differently
--   line 3: the division is not rounded
-- Converted draft:
CREATE FUNCTION shop.ratio() RETURNS numeric
LANGUAGE plpgsql
AS $body$
BEGIN
  RETURN 1 / 3::numeric;
END;
$body$;
-- The original on the source:
--   CREATE OR REPLACE FUNCTION ratio RETURN NUMBER IS
--   BEGIN
--     RETURN 1 / 3;
--   END;
--   /

-- function shop.hidden
-- the source account may not read its definition
-- (the source did not show its definition)

`
	if got != want {
		t.Errorf("script:\n%s\nwant:\n%s", got, want)
	}
}
