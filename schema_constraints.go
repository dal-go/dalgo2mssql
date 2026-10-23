package dalgo2mssql

import (
	"context"
	"database/sql"
	"strings"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

const constraintsSQL = `SELECT o.name, RTRIM(o.type) FROM sys.objects o
WHERE o.parent_object_id = @p1 AND o.type IN ('PK', 'UQ', 'F', 'C')
ORDER BY o.type, o.name`

const foreignKeysSQL = `SELECT f.name, pc.name, rs.name, rt.name, rc.name,
	f.update_referential_action_desc, f.delete_referential_action_desc, f.is_disabled
FROM sys.foreign_keys f
JOIN sys.foreign_key_columns fc ON fc.constraint_object_id = f.object_id
JOIN sys.columns pc ON pc.object_id = fc.parent_object_id AND pc.column_id = fc.parent_column_id
JOIN sys.objects rt ON rt.object_id = fc.referenced_object_id
JOIN sys.schemas rs ON rs.schema_id = rt.schema_id
JOIN sys.columns rc ON rc.object_id = fc.referenced_object_id AND rc.column_id = fc.referenced_column_id
WHERE f.parent_object_id = @p1
ORDER BY f.name, fc.constraint_column_id`

const referrersSQL = `SELECT po.name, f.name, pc.name
FROM sys.foreign_keys f
JOIN sys.foreign_key_columns fc ON fc.constraint_object_id = f.object_id
JOIN sys.objects po ON po.object_id = f.parent_object_id
JOIN sys.columns pc ON pc.object_id = fc.parent_object_id AND pc.column_id = fc.parent_column_id
WHERE f.referenced_object_id = @p1 AND po.schema_id = SCHEMA_ID(@p2) AND po.is_ms_shipped = 0
ORDER BY po.name, f.name, fc.constraint_column_id`

// constraintTypes maps a sys.objects constraint type to the type
// ListConstraints reports.
var constraintTypes = map[string]string{
	"PK": "primary-key",
	"UQ": "unique",
	"F":  "foreign-key",
	"C":  "check",
}

// ListConstraints returns the PRIMARY KEY, UNIQUE, FOREIGN KEY and CHECK
// constraints of a table, by name within type (types "check", "foreign-key",
// "primary-key", "unique"). A view has none.
func (d *Database) ListConstraints(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.ConstraintDef, error) {
	const op = "ListConstraints"
	id, err := d.lookup(ctx, op, ref)
	if err != nil {
		return nil, err
	}
	var out []dbschema.ConstraintDef
	err = queryRows(ctx, d.sqlDB, op, constraintsSQL, []any{id}, func(rows *sql.Rows) error {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			return err
		}
		out = append(out, dbschema.ConstraintDef{Name: name, Type: constraintTypes[kind]})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListReferrers returns one entry per foreign key that references the table,
// naming the referencing table and its columns in key order. Only referencing
// tables of the same schema are listed: a reference carries no schema.
func (d *Database) ListReferrers(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.Referrer, error) {
	const op = "ListReferrers"
	id, err := d.lookup(ctx, op, ref)
	if err != nil {
		return nil, err
	}
	var (
		out      []dbschema.Referrer
		lastKey  string
		lastFrom string
	)
	err = queryRows(ctx, d.sqlDB, op, referrersSQL, []any{id, d.schemaOf(ref)}, func(rows *sql.Rows) error {
		var table, key, column string
		if err := rows.Scan(&table, &key, &column); err != nil {
			return err
		}
		if len(out) == 0 || table != lastFrom || key != lastKey {
			out = append(out, dbschema.Referrer{Collection: dal.NewRootCollectionRef(table, "")})
			lastFrom, lastKey = table, key
		}
		out[len(out)-1].Fields = append(out[len(out)-1].Fields, dal.FieldName(column))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// readForeignKeys returns the foreign keys of the object id in schema. A
// key into another schema names it in ReferencedNamespace. A disabled key
// (NOCHECK) is reported with ForeignKeyEnforcementDisabled.
func (d *Database) readForeignKeys(ctx context.Context, op string, id int64, schema string) ([]dbschema.ForeignKeyDef, error) {
	var keys []dbschema.ForeignKeyDef
	err := queryRows(ctx, d.sqlDB, op, foreignKeysSQL, []any{id}, func(rows *sql.Rows) error {
		var (
			name, from, targetSchema, target, to, onUpdate, onDelete string
			disabled                                                 bool
		)
		if err := rows.Scan(&name, &from, &targetSchema, &target, &to, &onUpdate, &onDelete, &disabled); err != nil {
			return err
		}
		if n := len(keys); n == 0 || keys[n-1].Name != name {
			key := dbschema.ForeignKeyDef{
				Name:                 name,
				ReferencedCollection: target,
				Enforcement:          dbschema.ForeignKeyEnforcementEnabled,
				OnUpdate:             strings.ReplaceAll(onUpdate, "_", " "),
				OnDelete:             strings.ReplaceAll(onDelete, "_", " "),
			}
			if targetSchema != schema {
				key.ReferencedNamespace = targetSchema
			}
			if disabled {
				key.Enforcement = dbschema.ForeignKeyEnforcementDisabled
			}
			keys = append(keys, key)
		}
		key := &keys[len(keys)-1]
		key.Fields = append(key.Fields, dal.FieldName(from))
		key.ReferencedFields = append(key.ReferencedFields, dal.FieldName(to))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}
