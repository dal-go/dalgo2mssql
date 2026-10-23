package dalgo2mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	mssql "github.com/microsoft/go-mssqldb"
)

// ConnectionErrorKind says which step of reaching the server failed. It decides
// the fixed text of a [ConnectionError].
type ConnectionErrorKind string

const (
	// KindOpen: the driver refused the DSN (sql.Open failed).
	KindOpen ConnectionErrorKind = "open"
	// KindPing: the server could not be reached, refused the login, or the ping
	// timed out.
	KindPing ConnectionErrorKind = "ping"
	// KindSchema: the schema to read could not be resolved after connecting.
	KindSchema ConnectionErrorKind = "schema"
	// KindConnection: a later call failed with something other than an error
	// the server returned for its statement: a lost connection, a refused
	// login on reconnect, a timeout or a canceled context.
	KindConnection ConnectionErrorKind = "connection"
)

var kindText = map[ConnectionErrorKind]string{
	KindOpen:       "the connection string was refused by the SQL Server driver",
	KindPing:       "the SQL Server could not be reached or refused the connection",
	KindSchema:     "the default schema of the connection could not be read",
	KindConnection: "the connection to SQL Server failed",
}

// ConnectionError reports a failure to reach SQL Server. Its text is fixed by
// Kind (and Op, a name of this package's own call); it never holds the DSN,
// the user, the password, the host, the database name or the driver's
// message. The driver's error is reachable only through Unwrap.
type ConnectionError struct {
	Kind ConnectionErrorKind
	// Op is the call that failed, for example "ListCollections"; empty for the
	// steps of NewDatabase.
	Op    string
	cause error
}

func (e *ConnectionError) Error() string {
	text, ok := kindText[e.Kind]
	if !ok {
		text = kindText[KindConnection]
	}
	if e.Op != "" {
		return "dalgo2mssql: " + e.Op + ": " + text
	}
	return "dalgo2mssql: " + text
}

// Unwrap returns the driver's error.
func (e *ConnectionError) Unwrap() error { return e.cause }

// CatalogError reports a catalog query that SQL Server rejected (for example
// for lack of VIEW DEFINITION permission). Its text holds the call and the
// server's error number only: the server's message can name the database.
// The driver's error is reachable only through Unwrap.
type CatalogError struct {
	Op     string
	Number int32
	cause  error
}

func (e *CatalogError) Error() string {
	return fmt.Sprintf("dalgo2mssql: %s: catalog query failed (SQL Server error %d)", e.Op, e.Number)
}

// Unwrap returns the driver's error.
func (e *CatalogError) Unwrap() error { return e.cause }

// DriverError reports an error of the SQL Server driver that is neither a
// server's error for a statement nor a failure of the connection, for example
// a malformed reply. Its text holds the call only: the driver's message can
// name the server. The driver's error is reachable only through Unwrap.
type DriverError struct {
	Op    string
	cause error
}

func (e *DriverError) Error() string {
	return "dalgo2mssql: " + e.Op + ": the SQL Server driver failed"
}

// Unwrap returns the driver's error.
func (e *DriverError) Unwrap() error { return e.cause }

// ErrCollectionNotFound is matched (errors.Is) by the error a reader method
// returns for a table or view that is not in the schema.
var ErrCollectionNotFound = errors.New("collection not found")

// ErrSchemaNotFound is matched (errors.Is) by the error NewDatabase returns
// when the schema named with WithSchema does not exist.
var ErrSchemaNotFound = errors.New("schema not found")

func newCollectionNotFoundError(schema, name string) error {
	return fmt.Errorf("dalgo2mssql: %w: %s.%s", ErrCollectionNotFound, schema, name)
}

// loginErrors are server errors about the login or the database of the
// connection, whose messages name the user or the database: they are reported
// as connection failures.
var loginErrors = map[int32]bool{
	4060:  true, // cannot open database requested by the login
	4064:  true, // cannot open user default database
	18452: true, // login from an untrusted domain
	18456: true, // login failed for user
	18470: true, // login disabled
	18486: true, // account locked out
	18487: true, // password expired
	18488: true, // password must be changed
}

// classify turns an error of a later call into one that carries no driver
// text: a server error for the statement becomes a CatalogError, anything else
// a ConnectionError of kind KindConnection.
func classify(op string, err error) error {
	var serverErr mssql.Error
	if errors.As(err, &serverErr) && !loginErrors[serverErr.Number] {
		return &CatalogError{Op: op, Number: serverErr.Number, cause: err}
	}
	return &ConnectionError{Kind: KindConnection, Op: op, cause: err}
}

// queryRows runs query and calls scan for every row. Every failure is
// classified.
func queryRows(ctx context.Context, db *sql.DB, op, query string, args []any, scan func(*sql.Rows) error) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return classify(op, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err = scan(rows); err != nil {
			return classify(op, err)
		}
	}
	if err = rows.Err(); err != nil {
		return classify(op, err)
	}
	return nil
}
