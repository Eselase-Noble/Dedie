package mongodb

import (
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Text renders a BSON value for display and for storing as text: strings
// as they are, numbers and booleans in their usual form, ObjectIDs as hex,
// dates as RFC 3339, and anything structured as relaxed Extended JSON. A
// null or missing value is nil.
func Text(v bson.RawValue) *string {
	var out string
	switch v.Type {
	case bson.TypeNull, bson.TypeUndefined, 0:
		return nil
	case bson.TypeString:
		out = v.StringValue()
	case bson.TypeObjectID:
		out = v.ObjectID().Hex()
	case bson.TypeInt32:
		out = strconv.FormatInt(int64(v.Int32()), 10)
	case bson.TypeInt64:
		out = strconv.FormatInt(v.Int64(), 10)
	case bson.TypeDouble:
		out = strconv.FormatFloat(v.Double(), 'g', -1, 64)
	case bson.TypeBoolean:
		out = strconv.FormatBool(v.Boolean())
	case bson.TypeDateTime:
		out = time.UnixMilli(v.DateTime()).UTC().Format(time.RFC3339Nano)
	default:
		out = JSON(v)
	}
	return &out
}

// JSON renders any BSON value as relaxed Extended JSON.
func JSON(v bson.RawValue) string {
	// The library serializes documents, so the value is wrapped in one
	// and the wrapper cut away again.
	b, err := bson.MarshalExtJSON(bson.D{{Key: "v", Value: v}}, false, false)
	if err != nil {
		return v.String()
	}
	s := strings.TrimSuffix(strings.TrimPrefix(string(b), `{"v":`), "}")
	return strings.TrimSpace(s)
}

// DocumentJSON renders a whole document as relaxed Extended JSON.
func DocumentJSON(doc bson.Raw) (string, error) {
	b, err := bson.MarshalExtJSON(doc, false, false)
	return string(b), err
}

// TypeName is the name shown for a BSON type in the schema tree.
func TypeName(t bson.Type) string {
	switch t {
	case bson.TypeEmbeddedDocument:
		return "object"
	case bson.TypeDateTime:
		return "date"
	case bson.TypeInt32:
		return "int"
	case bson.TypeInt64:
		return "long"
	case bson.TypeDecimal128:
		return "decimal"
	case bson.TypeBoolean:
		return "bool"
	}
	return t.String()
}
