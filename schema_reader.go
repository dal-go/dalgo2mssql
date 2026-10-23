package dalgo2mssql

import (
	"context"
	"database/sql"
	"errors"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/record"
)

const listObjectsSQL = `SELECT o.name FROM sys.objects o
WHERE o.schema_id = SCHEMA_ID(@p1) AND o.type IN ('U', 'V') AND o.is_ms_shipped = 0
ORDER BY o.name`

const listViewsSQL = `SELECT o.name FROM sys.objects o
WHERE o.schema_id = SCHEMA_ID(@p1) AND o.type = 'V' AND o.is_ms_shipped = 0
ORDER BY o.name`

const lookupObjectSQL = `SELECT o.object_id FROM sys.objects o
WHERE o.schema_id = SCHEMA_ID(@p1) AND o.name = @p2 AND o.type IN ('U', 'V') AND o.is_ms_shipped = 0`

const columnsSQL = `SELECT c.name,
	CASE WHEN ty.user_type_id <> ty.system_type_id AND ty.is_assembly_type = 0 THEN TYPE_NAME(c.system_type_id) ELSE ty.name END,
	c.max_length, c.precision, c.scale, c.is_nullable, c.is_identity, dc.definition
FROM sys.columns c
JOIN sys.types ty ON ty.user_type_id = c.user_type_id
LEFT JOIN sys.default_constraints dc ON dc.object_id = c.default_object_id
WHERE c.object_id = @p1
ORDER BY c.column_id`

const primaryKeySQL = `SELECT c.name FROM sys.indexes i
JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE i.object_id = @p1 AND i.is_primary_key = 1
ORDER BY ic.key_ordinal`

const indexesSQL = `SELECT i.name, i.is_unique, c.name FROM sys.indexes i
JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE i.object_id = @p1 AND i.index_id > 0 AND i.is_primary_key = 0 AND i.is_hypothetical = 0
  AND ic.is_included_column = 0 AND ic.key_ordinal > 0
ORDER BY i.name, ic.key_ordinal`

// ListCollections returns the user tables and views of the schema, by name.
// parent is ignored: a schema is flat.
func (d *Database) ListCollections(ctx context.Context, _ *record.Key) ([]dal.CollectionRef, error) {
	return d.listNames(ctx, "ListCollections", listObjectsSQL)
}

// ListViews returns the views of the schema, by name; every one of them is
// also among what ListCollections returns. It lets a schema browser tell a
// view from a table.
func (d *Database) ListViews(ctx context.Context) ([]dal.CollectionRef, error) {
	return d.listNames(ctx, "ListViews", listViewsSQL)
}

func (d *Database) listNames(ctx context.Context, op, query string) ([]dal.CollectionRef, error) {
	var out []dal.CollectionRef
	err := queryRows(ctx, d.sqlDB, op, query, []any{d.schema}, func(rows *sql.Rows) error {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		out = append(out, dal.NewRootCollectionRef(name, ""))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// schemaOf returns the schema a reference names, else the configured one.
func (d *Database) schemaOf(ref *dal.CollectionRef) string {
	if s := ref.Schema(); s != "" {
		return s
	}
	return d.schema
}

// lookup returns the object_id of the table or view ref names.
func (d *Database) lookup(ctx context.Context, op string, ref *dal.CollectionRef) (int64, error) {
	schema := d.schemaOf(ref)
	var id int64
	err := d.sqlDB.QueryRowContext(ctx, lookupObjectSQL, schema, ref.Name()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, newCollectionNotFoundError(schema, ref.Name())
	}
	if err != nil {
		return 0, classify(op, err)
	}
	return id, nil
}

// DescribeCollection returns the columns (portable type, length, precision,
// nullability, default expression text, identity), the primary key in key
// order, the indexes and the foreign keys of a table or view.
func (d *Database) DescribeCollection(ctx context.Context, ref *dal.CollectionRef) (*dbschema.CollectionDef, error) {
	const op = "DescribeCollection"
	id, err := d.lookup(ctx, op, ref)
	if err != nil {
		return nil, err
	}
	var pk []dal.FieldName
	err = queryRows(ctx, d.sqlDB, op, primaryKeySQL, []any{id}, func(rows *sql.Rows) error {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		pk = append(pk, dal.FieldName(name))
		return nil
	})
	if err != nil {
		return nil, err
	}
	inPK := make(map[dal.FieldName]bool, len(pk))
	for _, name := range pk {
		inPK[name] = true
	}
	var fields []dbschema.FieldDef
	err = queryRows(ctx, d.sqlDB, op, columnsSQL, []any{id}, func(rows *sql.Rows) error {
		var (
			name, typeName     string
			maxLength          int64
			precision, scale   int64
			nullable, identity bool
			definition         sql.NullString
		)
		if err := rows.Scan(&name, &typeName, &maxLength, &precision, &scale, &nullable, &identity, &definition); err != nil {
			return err
		}
		t, length, prec := portableType(typeName, maxLength, precision, scale)
		field := dbschema.FieldDef{
			Name:          dal.FieldName(name),
			Type:          t,
			Length:        length,
			Precision:     prec,
			Nullable:      nullable && !inPK[dal.FieldName(name)],
			AutoIncrement: identity,
		}
		if definition.Valid {
			field.Default = dbschema.DefaultLiteral{Value: stripOuterParens(definition.String)}
		}
		fields = append(fields, field)
		return nil
	})
	if err != nil {
		return nil, err
	}
	indexes, err := d.listIndexes(ctx, op, id, ref.Name())
	if err != nil {
		return nil, err
	}
	foreignKeys, err := d.readForeignKeys(ctx, op, id, d.schemaOf(ref))
	if err != nil {
		return nil, err
	}
	return &dbschema.CollectionDef{
		Name:        ref.Name(),
		Fields:      fields,
		PrimaryKey:  pk,
		Indexes:     indexes,
		ForeignKeys: foreignKeys,
	}, nil
}

// ListIndexes returns the indexes of a table or view other than its primary
// key, each with its key columns in key order (included columns are left
// out). The index behind a UNIQUE constraint is listed under the constraint's
// name, unique.
func (d *Database) ListIndexes(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.IndexDef, error) {
	const op = "ListIndexes"
	id, err := d.lookup(ctx, op, ref)
	if err != nil {
		return nil, err
	}
	return d.listIndexes(ctx, op, id, ref.Name())
}

func (d *Database) listIndexes(ctx context.Context, op string, id int64, collection string) ([]dbschema.IndexDef, error) {
	var out []dbschema.IndexDef
	err := queryRows(ctx, d.sqlDB, op, indexesSQL, []any{id}, func(rows *sql.Rows) error {
		var (
			name, column string
			unique       bool
		)
		if err := rows.Scan(&name, &unique, &column); err != nil {
			return err
		}
		if n := len(out); n == 0 || out[n-1].Name != name {
			out = append(out, dbschema.IndexDef{Name: name, Collection: collection, Unique: unique})
		}
		out[len(out)-1].Fields = append(out[len(out)-1].Fields, dal.FieldName(column))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// stripOuterParens removes the parentheses SQL Server wraps a default
// definition in: ((0)) is 0, (getdate()) is getdate(). A pair is removed only
// when the first parenthesis closes at the end of the text.
func stripOuterParens(s string) string {
	for len(s) >= 2 && s[0] == '(' && closingParen(s) == len(s)-1 {
		s = s[1 : len(s)-1]
	}
	return s
}

// closingParen returns the index of the parenthesis that closes the one at
// s[0], skipping quoted strings, or -1.
func closingParen(s string) int {
	depth := 0
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			inQuote = !inQuote
		case inQuote:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
