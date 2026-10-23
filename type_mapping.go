package dalgo2mssql

import (
	"strings"

	"github.com/dal-go/dalgo/dbschema"
)

// portableType maps a SQL Server type (the system type of an alias type) to a
// dbschema type, with its length for a string or binary type of bounded size
// (nil for MAX and for the large-object types) and its precision for an exact
// numeric type. A type with no closer portable type is a string.
func portableType(typeName string, maxLength, precision, scale int64) (dbschema.Type, *int, *dbschema.Precision) {
	switch strings.ToLower(typeName) {
	case "bit":
		return dbschema.Bool, nil, nil
	case "tinyint", "smallint", "int", "bigint":
		return dbschema.Int, nil, nil
	case "real", "float":
		return dbschema.Float, nil, nil
	case "decimal", "numeric", "money", "smallmoney":
		return dbschema.Decimal, nil, &dbschema.Precision{Total: int(precision), Scale: int(scale)}
	case "char", "varchar":
		return dbschema.String, boundedLength(maxLength, 1), nil
	case "nchar", "nvarchar":
		return dbschema.String, boundedLength(maxLength, 2), nil
	case "binary", "varbinary":
		return dbschema.Bytes, boundedLength(maxLength, 1), nil
	case "image", "timestamp", "rowversion", "hierarchyid", "geography", "geometry":
		return dbschema.Bytes, nil, nil
	case "date", "time", "datetime", "datetime2", "smalldatetime", "datetimeoffset":
		return dbschema.Time, nil, nil
	default: // text, ntext, xml, uniqueidentifier, sql_variant, …
		return dbschema.String, nil, nil
	}
}

// boundedLength is max_length in characters (bytes divided by bytesPerChar),
// or nil for MAX (-1).
func boundedLength(maxLength int64, bytesPerChar int64) *int {
	if maxLength < 0 {
		return nil
	}
	n := int(maxLength / bytesPerChar)
	return &n
}
