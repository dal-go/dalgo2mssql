package dalgo2mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo2sql"

	_ "github.com/microsoft/go-mssqldb" // registers the "sqlserver" driver
)

// DriverName is the database/sql driver name the package opens.
const DriverName = "sqlserver"

// StructuredQueryDialect is the dalgo2sql structured-query dialect the
// package sets on the dalgo2sql options.
const StructuredQueryDialect = "sqlserver"

// Version is the dalgo2mssql adapter version, reported by Adapter.
const Version = "0.1.0"

// sqlOpen is the test seam for sql.Open.
var sqlOpen = sql.Open

var (
	_ dal.DB                = (*Database)(nil)
	_ dbschema.SchemaReader = (*Database)(nil)
	_ io.Closer             = (*Database)(nil)
	_ interface {
		ListViews(context.Context) ([]dal.CollectionRef, error)
	} = (*Database)(nil)
)

// Database is a SQL Server database: a dal.DB (the embedded dalgo2sql
// database) that also reads the schema of one SQL Server schema.
type Database struct {
	dal.DB
	sqlDB  *sql.DB
	schema string
}

// Option configures NewDatabase.
type Option func(*options)

type options struct {
	schema    string
	dbOptions dalgo2sql.DbOptions
	dalSchema dal.Schema
}

// WithSchema names the SQL Server schema to read, instead of the default
// schema of the login (SCHEMA_NAME()).
func WithSchema(name string) Option {
	return func(o *options) { o.schema = name }
}

// WithDbOptions sets the dalgo2sql options of the embedded dal.DB. Its
// StructuredQueryDialect is always replaced with "sqlserver".
func WithDbOptions(dbOptions dalgo2sql.DbOptions) Option {
	return func(o *options) { o.dbOptions = dbOptions }
}

// WithDalSchema sets the dal.Schema of the embedded dal.DB (an empty one by
// default).
func WithDalSchema(schema dal.Schema) Option {
	return func(o *options) { o.dalSchema = schema }
}

// NewDatabase opens the SQL Server named by dsn (sqlserver://… form), pings it
// with ctx and resolves the schema to read. Failures are *ConnectionError
// values whose text never holds any part of dsn; a schema named with
// WithSchema that does not exist matches ErrSchemaNotFound.
func NewDatabase(ctx context.Context, dsn string, opts ...Option) (*Database, error) {
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}
	if o.dalSchema == nil {
		o.dalSchema = dal.NewSchema(nil, nil)
	}
	o.dbOptions.StructuredQueryDialect = StructuredQueryDialect

	sqlDB, err := sqlOpen(DriverName, dsn)
	if err != nil {
		return nil, &ConnectionError{Kind: KindOpen, cause: err}
	}
	if err = sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, &ConnectionError{Kind: KindPing, cause: err}
	}
	schema, err := resolveSchema(ctx, sqlDB, o.schema)
	if err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return &Database{
		DB:     dalgo2sql.NewDatabase(sqlDB, o.dalSchema, o.dbOptions),
		sqlDB:  sqlDB,
		schema: schema,
	}, nil
}

// resolveSchema returns the login's default schema when name is empty, and
// checks that a named schema exists.
func resolveSchema(ctx context.Context, db *sql.DB, name string) (string, error) {
	if name == "" {
		var schema sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT SCHEMA_NAME()`).Scan(&schema); err != nil {
			return "", &ConnectionError{Kind: KindSchema, cause: err}
		}
		if !schema.Valid {
			return "", &ConnectionError{Kind: KindSchema, cause: ErrSchemaNotFound}
		}
		return schema.String, nil
	}
	var found string
	err := db.QueryRowContext(ctx, `SELECT s.name FROM sys.schemas s WHERE s.name = @p1`, name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("dalgo2mssql: %w: %s", ErrSchemaNotFound, name)
	}
	if err != nil {
		return "", &ConnectionError{Kind: KindSchema, cause: err}
	}
	return found, nil
}

// SchemaName returns the SQL Server schema the reader reads.
func (d *Database) SchemaName() string { return d.schema }

// Adapter names this adapter.
func (d *Database) Adapter() dal.Adapter { return dal.NewAdapter("dalgo2mssql", Version) }

// Close closes the connection pool.
func (d *Database) Close() error { return d.sqlDB.Close() }
