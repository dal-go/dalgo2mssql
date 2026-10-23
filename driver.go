package dalgo2mssql

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"reflect"
	"syscall"

	mssql "github.com/microsoft/go-mssqldb"
)

// The interfaces database/sql uses of a go-mssqldb connection, statement and
// result set. The wrappers below implement exactly these and delegate them, so
// nothing of the driver's behavior is lost and nothing needs a fallback.
type (
	mssqlConn interface {
		driver.Conn
		driver.ConnBeginTx
		driver.ConnPrepareContext
		driver.Pinger
		driver.SessionResetter
		driver.Validator
		driver.NamedValueChecker
	}
	mssqlStmt interface {
		driver.Stmt
		driver.StmtExecContext
		driver.StmtQueryContext
	}
	mssqlRows interface {
		driver.Rows
		driver.RowsNextResultSet
		driver.RowsColumnTypeDatabaseTypeName
		driver.RowsColumnTypeLength
		driver.RowsColumnTypeNullable
		driver.RowsColumnTypePrecisionScale
		driver.RowsColumnTypeScanType
	}
)

var (
	_ mssqlConn = (*mssql.Conn)(nil)
	_ mssqlStmt = (*mssql.Stmt)(nil)
	_ mssqlRows = (*mssql.Rows)(nil)

	_ mssqlConn = (*conn)(nil)
	_ mssqlStmt = (*stmt)(nil)
	_ mssqlRows = (*rows)(nil)
)

// newMSSQLConnector is go-mssqldb's connector constructor, a seam so tests run
// without a server.
var newMSSQLConnector = func(dsn string) (driver.Connector, error) {
	c, err := mssql.NewConnector(dsn)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ErrReadOnlyTransaction is returned when a transaction is begun read-only
// (sql.TxOptions{ReadOnly: true}, dal's RunReadonlyTransaction): SQL Server has
// no read-only transaction, and this package does not fake one.
var ErrReadOnlyTransaction = errors.New("dalgo2mssql: SQL Server has no read-only transactions; use a read-only login")

// errUnsupportedDriverType is returned when the driver hands out a connection,
// statement or result set that lacks an interface go-mssqldb's own types have,
// for example the statement of a bulk copy (mssql.CopyIn).
var errUnsupportedDriverType = errors.New("dalgo2mssql: the driver returned an object this package does not support")

// NewConnector returns a driver.Connector for dsn (sqlserver://user:pass@host:1433?database=shop)
// that a caller can open with sql.OpenDB. It is go-mssqldb's connector with
// every error it returns sanitised (see the package documentation, Errors):
//
//   - a failure to connect, a login or database error the server returns
//     (18456, 4060 and the like) and a failure of the transport are a
//     *ConnectionError;
//   - an error the server returns for a statement is go-mssqldb's own
//     mssql.Error, unchanged;
//   - any other error of the driver is a *DriverError;
//   - database/sql's driver.ErrBadConn and the context errors are returned
//     bare, so database/sql still retries on a bad connection.
//
// A transaction begun read-only fails with ErrReadOnlyTransaction.
//
// A DSN the driver refuses is a *ConnectionError of kind KindOpen.
func NewConnector(dsn string) (driver.Connector, error) {
	inner, err := newMSSQLConnector(dsn)
	if err != nil {
		return nil, &ConnectionError{Kind: KindOpen, cause: err}
	}
	return &connector{inner: inner}, nil
}

type connector struct{ inner driver.Connector }

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	inner, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, &ConnectionError{Kind: KindConnection, Op: "connect", cause: err}
	}
	mc, ok := inner.(mssqlConn)
	if !ok {
		_ = inner.Close()
		return nil, errUnsupportedDriverType
	}
	return &conn{inner: mc}, nil
}

func (c *connector) Driver() driver.Driver { return c.inner.Driver() }

type conn struct{ inner mssqlConn }

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	s, err := c.inner.PrepareContext(ctx, query)
	if err != nil {
		return nil, sanitize("prepare", err)
	}
	ms, ok := s.(mssqlStmt)
	if !ok {
		_ = s.Close()
		return nil, errUnsupportedDriverType
	}
	return &stmt{inner: ms}, nil
}

func (c *conn) Close() error { return sanitize("close", c.inner.Close()) }

func (c *conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if opts.ReadOnly {
		return nil, ErrReadOnlyTransaction
	}
	t, err := c.inner.BeginTx(ctx, opts)
	if err != nil {
		return nil, sanitize("begin", err)
	}
	return &tx{inner: t}, nil
}

func (c *conn) Ping(ctx context.Context) error { return sanitize("ping", c.inner.Ping(ctx)) }

func (c *conn) ResetSession(ctx context.Context) error {
	return sanitize("reset session", c.inner.ResetSession(ctx))
}

func (c *conn) IsValid() bool { return c.inner.IsValid() }

// CheckNamedValue is delegated unchanged: it only converts an argument, and
// database/sql relies on its driver.ErrSkip and driver.ErrRemoveArgument.
func (c *conn) CheckNamedValue(nv *driver.NamedValue) error { return c.inner.CheckNamedValue(nv) }

type tx struct{ inner driver.Tx }

func (t *tx) Commit() error   { return sanitize("commit", t.inner.Commit()) }
func (t *tx) Rollback() error { return sanitize("rollback", t.inner.Rollback()) }

type stmt struct{ inner mssqlStmt }

func (s *stmt) Close() error  { return sanitize("close statement", s.inner.Close()) }
func (s *stmt) NumInput() int { return s.inner.NumInput() }

func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	res, err := s.inner.Exec(args) //nolint:staticcheck // delegated
	return res, sanitize("exec", err)
}

func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	r, err := s.inner.Query(args) //nolint:staticcheck // delegated
	return wrapRows(r, err)
}

func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	res, err := s.inner.ExecContext(ctx, args)
	return res, sanitize("exec", err)
}

func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	r, err := s.inner.QueryContext(ctx, args)
	return wrapRows(r, err)
}

func wrapRows(r driver.Rows, err error) (driver.Rows, error) {
	if err != nil {
		return nil, sanitize("query", err)
	}
	mr, ok := r.(mssqlRows)
	if !ok {
		_ = r.Close()
		return nil, errUnsupportedDriverType
	}
	return &rows{inner: mr}, nil
}

// rows wraps a result set: a fetch can fail on the transport.
type rows struct{ inner mssqlRows }

func (r *rows) Columns() []string { return r.inner.Columns() }
func (r *rows) Close() error      { return sanitize("close rows", r.inner.Close()) }

func (r *rows) Next(dest []driver.Value) error {
	return endOrSanitized("fetch", r.inner.Next(dest))
}

func (r *rows) HasNextResultSet() bool { return r.inner.HasNextResultSet() }

func (r *rows) NextResultSet() error {
	return endOrSanitized("next result set", r.inner.NextResultSet())
}

func (r *rows) ColumnTypeDatabaseTypeName(index int) string {
	return r.inner.ColumnTypeDatabaseTypeName(index)
}

func (r *rows) ColumnTypeLength(index int) (int64, bool) { return r.inner.ColumnTypeLength(index) }

func (r *rows) ColumnTypeNullable(index int) (bool, bool) {
	return r.inner.ColumnTypeNullable(index)
}

func (r *rows) ColumnTypePrecisionScale(index int) (int64, int64, bool) {
	return r.inner.ColumnTypePrecisionScale(index)
}

func (r *rows) ColumnTypeScanType(index int) reflect.Type { return r.inner.ColumnTypeScanType(index) }

// endOrSanitized returns io.EOF, the driver contract's end of a sequence, bare,
// and sanitises any other error.
func endOrSanitized(op string, err error) error {
	if err == io.EOF { //nolint:errorlint // the driver contract's own sentinel, returned bare
		return io.EOF
	}
	return sanitize(op, err)
}

// sanitize is applied to every error the driver returns once a connection is
// open. It decides by the types in the error's chain, never by its text.
func sanitize(op string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, driver.ErrSkip):
		return driver.ErrSkip
	case errors.Is(err, driver.ErrBadConn):
		return driver.ErrBadConn
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	}
	var serverErr mssql.Error
	if errors.As(err, &serverErr) {
		if isLoginError(serverErr) {
			return &ConnectionError{Kind: KindConnection, Op: op, cause: err}
		}
		return err
	}
	if isTransport(err) {
		return &ConnectionError{Kind: KindConnection, Op: op, cause: err}
	}
	return &DriverError{Op: op, cause: err}
}

// isLoginError reports a server error about the login or the database of the
// connection, in the error or in any message before it.
func isLoginError(e mssql.Error) bool {
	if loginErrors[e.Number] {
		return true
	}
	for _, m := range e.All {
		if loginErrors[m.Number] {
			return true
		}
	}
	return false
}

// isTransport reports a failure of the transport: an error of package net, a
// system call's error number, or the end of the stream.
func isTransport(err error) bool {
	var netErr net.Error
	var errno syscall.Errno
	return errors.As(err, &netErr) || errors.As(err, &errno) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
