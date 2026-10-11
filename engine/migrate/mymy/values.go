package mymy

// charTypes hold text in the column's own character set. The binary log
// carries their bytes as stored, so they are passed on as bytes and the
// target stores them unchanged.
var charTypes = map[string]bool{
	"char": true, "varchar": true, "tinytext": true, "text": true, "mediumtext": true, "longtext": true,
}

// Integer returns an integer decoded from a row event with its sign
// corrected: the binary log has no sign information, so unsigned values
// arrive as negative numbers once they pass the signed maximum. Other values
// are returned unchanged.
func Integer(c Column, v any) any {
	switch x := v.(type) {
	case int8:
		if c.Unsigned {
			return uint64(uint8(x))
		}
		return int64(x)
	case int16:
		if c.Unsigned {
			return uint64(uint16(x))
		}
		return int64(x)
	case int32:
		if c.Unsigned {
			if c.DataType == "mediumint" {
				return uint64(uint32(x) & 0xFFFFFF)
			}
			return uint64(uint32(x))
		}
		return int64(x)
	case int64:
		if c.Unsigned && c.DataType == "bigint" {
			return uint64(x)
		}
		return x
	case int:
		return int64(x)
	// Servers that log column signedness let the reader return unsigned
	// values directly; widen them to one type for the callers.
	case uint8:
		return uint64(x)
	case uint16:
		return uint64(x)
	case uint32:
		return uint64(x)
	case uint:
		return uint64(x)
	}
	return v
}

// binlogValue converts a value decoded from a row event into something the
// SQL driver will send in a form the target column accepts.
func binlogValue(c Column, v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		// JSON arrives as text and must be sent as text; a binary string is
		// not accepted by a JSON column.
		if c.DataType == "json" {
			return string(x)
		}
		return x
	case string:
		if charTypes[c.DataType] {
			return []byte(x)
		}
		return x
	case float32:
		return float64(x)
	}
	return Integer(c, v)
}
