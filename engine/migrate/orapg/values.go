package orapg

import (
	"encoding/hex"
	"fmt"
	"strings"

	"dedie/engine/migrate/orasrc"
)

// pgText converts a value read with orasrc.SelectExpr into the text PostgreSQL
// parses for the target column. The second result is false for NULL.
func pgText(c Column, v any) (string, bool) {
	var s string
	switch x := v.(type) {
	case nil:
		return "", false
	case []byte:
		if c.Kind == orasrc.KindBinary {
			return `\x` + hex.EncodeToString(x), true
		}
		s = string(x)
	case string:
		s = x
	default:
		s = fmt.Sprint(x)
	}
	switch c.Kind {
	case orasrc.KindIntervalYM, orasrc.KindIntervalDS:
		return orasrc.ISOInterval(c.Kind, s), true
	case orasrc.KindText, orasrc.KindXML, orasrc.KindJSON:
		// PostgreSQL text cannot contain the NUL character.
		if strings.IndexByte(s, 0) >= 0 {
			s = strings.ReplaceAll(s, "\x00", "")
		}
	}
	return s, true
}

// copyEscaper escapes a value for COPY's text format.
var copyEscaper = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`)

func copyField(c Column, v any) string {
	s, ok := pgText(c, v)
	if !ok {
		return `\N`
	}
	return copyEscaper.Replace(s)
}
