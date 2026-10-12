package viewsql

import (
	"fmt"
	"regexp"
	"strings"
)

// cast translates PostgreSQL's value::type. The value is whatever was
// written last. PostgreSQL adds such casts to nearly every literal when it
// stores a view, so most of them say nothing and are dropped; the rest
// become a CAST.
func (s *seq) cast() error {
	p := s.last()
	if p == nil || p.kind != pPrimary {
		return unsupported("it has a cast (::) on an expression this translator cannot follow")
	}
	typ, sizes, next, err := postgresType(s.nodes, s.i+1)
	if err != nil {
		return err
	}
	out, err := castToMySQL(*p, typ, sizes)
	if err != nil {
		return err
	}
	*p = out
	s.i = next
	return nil
}

// postgresType reads a type name starting at nodes[i]: one word, or one
// of the standard names of several ("character varying", "timestamp
// without time zone"), with its sizes. It returns the position after it.
func postgresType(nodes []node, i int) (name string, sizes []int, next int, err error) {
	word := func(j int) string {
		if j < len(nodes) && nodes[j].isName() {
			return strings.ToLower(nodes[j].tok.text)
		}
		return ""
	}
	if name = word(i); name == "" {
		return "", nil, 0, unsupported("it has a cast (::) to something this translator cannot read")
	}
	i++
	if i < len(nodes) && nodes[i].isOp(".") {
		if name != "pg_catalog" || word(i+1) == "" {
			return "", nil, 0, unsupported("it casts to %s.%s, a type defined in the source database", name, word(i+1))
		}
		name = word(i + 1)
		i += 2
	}
	readSizes := func() error {
		if i < len(nodes) && nodes[i].isGroup('(') && sizes == nil {
			var err error
			if sizes, err = typeSizes(&nodes[i]); err != nil {
				return err
			}
			i++
		}
		return nil
	}
	switch name {
	case "character", "bit":
		if word(i) == "varying" {
			name += " varying"
			i++
		}
	case "double":
		if word(i) == "precision" {
			name += " precision"
			i++
		}
	case "timestamp", "time":
		// The precision stands in the middle: timestamp(3) without time zone.
		if err := readSizes(); err != nil {
			return "", nil, 0, err
		}
		if w := word(i); (w == "with" || w == "without") && word(i+1) == "time" && word(i+2) == "zone" {
			name += " " + w + " time zone"
			i += 3
		}
	}
	if err := readSizes(); err != nil {
		return "", nil, 0, err
	}
	if i < len(nodes) && nodes[i].isGroup('[') {
		return "", nil, 0, unsupported("it uses an array")
	}
	return name, sizes, i, nil
}

var wholeNumberRe = regexp.MustCompile(`^\(*[0-9]+\)*$`)

// castToMySQL applies a PostgreSQL cast to a translated value.
func castToMySQL(p piece, typ string, sizes []int) (piece, error) {
	if p.lit == nullLiteral {
		// NULL is NULL in any type.
		return p, nil
	}
	as := func(mysqlType string) (piece, error) {
		return piece{kind: pPrimary, text: "CAST(" + p.text + " AS " + mysqlType + ")", temporal: p.temporal || strings.HasPrefix(mysqlType, "DATE")}, nil
	}
	switch typ {
	case "text", "character varying", "varchar", "character", "char", "bpchar", "name":
		if p.lit == stringLiteral {
			return p, nil
		}
		// A cast to CHAR takes the collation of the session, which is
		// usually one that ignores case. PostgreSQL compares text exactly,
		// and so do the migrated tables, which are created with a binary
		// collation; the cast has to keep to that.
		return piece{kind: pPrimary, text: "(CAST(" + p.text + " AS CHAR CHARACTER SET utf8mb4) COLLATE utf8mb4_bin)", temporal: p.temporal}, nil
	case "integer", "int4", "int", "bigint", "int8", "smallint", "int2":
		if p.lit == numberLiteral && wholeNumberRe.MatchString(p.text) {
			return p, nil
		}
		return as("SIGNED")
	case "numeric", "decimal":
		switch {
		case len(sizes) == 0 && p.lit == numberLiteral:
			// A number written out is exact in MySQL as it is.
			return p, nil
		case len(sizes) == 0:
			return as("DECIMAL(65,30)")
		}
		precision, scale := sizes[0], 0
		if len(sizes) > 1 {
			scale = sizes[1]
		}
		if precision > 65 || scale > 30 {
			return piece{}, unsupported("it casts to a number with more digits than MySQL's DECIMAL has")
		}
		return as(fmt.Sprintf("DECIMAL(%d,%d)", precision, scale))
	case "double precision", "float8":
		return as("DOUBLE")
	case "real", "float4":
		return as("FLOAT")
	case "date":
		return as("DATE")
	case "timestamp", "timestamp without time zone":
		digits := 6
		if len(sizes) == 1 {
			digits = sizes[0]
		}
		return as(fmt.Sprintf("DATETIME(%d)", digits))
	}
	return piece{}, unsupported("it casts to %s", typ)
}
