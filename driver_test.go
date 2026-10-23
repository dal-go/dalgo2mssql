package dalgo2mssql

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"syscall"
	"testing"

	"github.com/dal-go/dalgo/dal"
	mssql "github.com/microsoft/go-mssqldb"
)

// Errors as go-mssqldb returns them, each naming a secret of secretDSN.
var (
	errDial = fmt.Errorf("unable to open tcp connection with host 'db-host.example:1433': %w",
		&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")})
	errLogin = mssql.Error{Number: 18456, Message: "Login failed for user 'reader_login'."}
	// A login error that is not the last message of the reply.
	errLoginChain = mssql.Error{Number: 2, Message: "after", All: []mssql.Error{
		{Number: 4060, Message: `Cannot open database "shopdb" requested by the login.`},
		{Number: 2, Message: "after"},
	}}
	errOddReply  = errors.New("mssql: unexpected token from db-host.example")
	errStatement = mssql.Error{Number: 208, Message: "Invalid object name 'nope'."}
)

// ---- fakes: "full" has every interface a go-mssqldb type has, "bare" only the required one ----

type bareRows struct {
	values   [][]driver.Value
	nextErr  error
	closeErr error
}

func (r *bareRows) Columns() []string { return []string{"a"} }
func (r *bareRows) Close() error      { return r.closeErr }
func (r *bareRows) Next(dest []driver.Value) error {
	if len(r.values) == 0 {
		if r.nextErr != nil {
			return r.nextErr
		}
		return io.EOF
	}
	copy(dest, r.values[0])
	r.values = r.values[1:]
	return nil
}

type fullRows struct {
	bareRows
	nextSetErr error
}

func (r *fullRows) HasNextResultSet() bool                            { return true }
func (r *fullRows) NextResultSet() error                              { return r.nextSetErr }
func (r *fullRows) ColumnTypeDatabaseTypeName(int) string             { return "NVARCHAR" }
func (r *fullRows) ColumnTypeLength(int) (int64, bool)                { return 7, true }
func (r *fullRows) ColumnTypeNullable(int) (bool, bool)               { return true, true }
func (r *fullRows) ColumnTypePrecisionScale(int) (int64, int64, bool) { return 10, 2, true }
func (r *fullRows) ColumnTypeScanType(int) reflect.Type               { return reflect.TypeOf("") }

type fakeResult struct{}

func (fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (fakeResult) RowsAffected() (int64, error) { return 1, nil }

type bareStmt struct{ closeErr error }

func (s *bareStmt) Close() error                               { return s.closeErr }
func (s *bareStmt) NumInput() int                              { return -1 }
func (s *bareStmt) Exec([]driver.Value) (driver.Result, error) { return fakeResult{}, nil }
func (s *bareStmt) Query([]driver.Value) (driver.Rows, error)  { return &bareRows{}, nil }

// fullStmt answers with the conn's script for its query.
type fullStmt struct {
	bareStmt
	query string
	conn  *fullConn
}

func (s *fullStmt) Exec([]driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), nil)
}
func (s *fullStmt) Query([]driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), nil)
}
func (s *fullStmt) ExecContext(context.Context, []driver.NamedValue) (driver.Result, error) {
	if s.conn.execErr != nil {
		return nil, s.conn.execErr
	}
	return fakeResult{}, nil
}
func (s *fullStmt) QueryContext(context.Context, []driver.NamedValue) (driver.Rows, error) {
	return s.conn.query(s.query)
}

type fakeTx struct{ err error }

func (t *fakeTx) Commit() error   { return t.err }
func (t *fakeTx) Rollback() error { return t.err }

type bareConn struct{ closed bool }

func (c *bareConn) Prepare(string) (driver.Stmt, error) { return &bareStmt{}, nil }
func (c *bareConn) Close() error                        { c.closed = true; return nil }
func (c *bareConn) Begin() (driver.Tx, error)           { return &fakeTx{}, nil }

type fullConn struct {
	bareConn
	// query answers a statement's query; nil answers an empty result set.
	query     func(q string) (driver.Rows, error)
	prepare   func(q string) (driver.Stmt, error)
	execErr   error
	err       error // of Ping, ResetSession, BeginTx and Close
	tx        *fakeTx
	beginOpts driver.TxOptions
}

func (c *fullConn) Prepare(q string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), q)
}
func (c *fullConn) PrepareContext(_ context.Context, q string) (driver.Stmt, error) {
	if c.prepare != nil {
		return c.prepare(q)
	}
	if c.query == nil {
		c.query = func(string) (driver.Rows, error) { return &fullRows{}, nil }
	}
	return &fullStmt{query: q, conn: c}, nil
}
func (c *fullConn) Close() error { return c.err }
func (c *fullConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.beginOpts = opts
	if c.err != nil {
		return nil, c.err
	}
	if c.tx == nil {
		c.tx = &fakeTx{}
	}
	return c.tx, nil
}
func (c *fullConn) Ping(context.Context) error         { return c.err }
func (c *fullConn) ResetSession(context.Context) error { return c.err }
func (c *fullConn) IsValid() bool                      { return true }
func (c *fullConn) CheckNamedValue(nv *driver.NamedValue) error {
	if nv.Value == "remove" {
		return driver.ErrRemoveArgument
	}
	return driver.ErrSkip
}

type fakeConnector struct {
	conn driver.Conn
	err  error
}

func (f *fakeConnector) Connect(context.Context) (driver.Conn, error) { return f.conn, f.err }
func (f *fakeConnector) Driver() driver.Driver                        { return nil }

// ---- tests ----

func TestNewConnector(t *testing.T) {
	c, err := NewConnector(secretDSN)
	if err != nil {
		t.Fatal(err)
	}
	if c.Driver() == nil {
		t.Fatal("want go-mssqldb's driver")
	}
	_, err = NewConnector("sqlserver://reader_login:S3cretPassw0rd@db-host.example:notaport?database=shopdb")
	var connErr *ConnectionError
	if !errors.As(err, &connErr) || connErr.Kind != KindOpen {
		t.Fatalf("got %v", err)
	}
	assertNoSecrets(t, err)
}

func TestConnector_Connect(t *testing.T) {
	ctx := context.Background()
	if c, err := (&connector{inner: &fakeConnector{conn: &fullConn{}}}).Connect(ctx); err != nil || c.(*conn).inner == nil {
		t.Fatalf("Connect: %v", err)
	}
	for _, cause := range []error{errDial, errLogin, context.DeadlineExceeded} {
		_, err := (&connector{inner: &fakeConnector{err: cause}}).Connect(ctx)
		var connErr *ConnectionError
		if !errors.As(err, &connErr) || connErr.Kind != KindConnection || connErr.Op != "connect" || errors.Unwrap(err) == nil {
			t.Fatalf("got %#v", err)
		}
		assertNoSecrets(t, err)
	}
	bare := &bareConn{}
	if _, err := (&connector{inner: &fakeConnector{conn: bare}}).Connect(ctx); !errors.Is(err, errUnsupportedDriverType) || !bare.closed {
		t.Fatalf("got %v, closed %v", err, bare.closed)
	}
}

func TestConn(t *testing.T) {
	ctx := context.Background()
	fc := &fullConn{}
	c := &conn{inner: fc}

	if _, err := c.Prepare("q"); err != nil {
		t.Fatal(err)
	}
	fc.prepare = func(string) (driver.Stmt, error) { return nil, errDial }
	_, err := c.PrepareContext(ctx, "q")
	assertNoSecrets(t, err)
	bs := &bareStmt{closeErr: errors.New("ignored")}
	fc.prepare = func(string) (driver.Stmt, error) { return bs, nil }
	if _, err = c.PrepareContext(ctx, "q"); !errors.Is(err, errUnsupportedDriverType) {
		t.Fatalf("got %v", err)
	}

	tx, err := c.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if tx.Commit() != nil || tx.Rollback() != nil {
		t.Fatal("tx")
	}
	fc.tx.err = errDial
	assertNoSecrets(t, tx.Commit())
	assertNoSecrets(t, tx.Rollback())
	if _, err = c.BeginTx(ctx, driver.TxOptions{ReadOnly: true}); !errors.Is(err, ErrReadOnlyTransaction) {
		t.Fatalf("got %v", err)
	}
	if _, err = c.BeginTx(ctx, driver.TxOptions{Isolation: 2}); err != nil || fc.beginOpts.Isolation != 2 {
		t.Fatalf("isolation not passed on: %v", err)
	}

	if c.Ping(ctx) != nil || c.ResetSession(ctx) != nil || c.Close() != nil || !c.IsValid() {
		t.Fatal("delegation")
	}
	if !errors.Is(c.CheckNamedValue(&driver.NamedValue{}), driver.ErrSkip) ||
		!errors.Is(c.CheckNamedValue(&driver.NamedValue{Value: "remove"}), driver.ErrRemoveArgument) {
		t.Fatal("CheckNamedValue")
	}

	fc.err = errLogin
	_, err = c.BeginTx(ctx, driver.TxOptions{})
	assertNoSecrets(t, err)
	assertNoSecrets(t, c.Ping(ctx))
	assertNoSecrets(t, c.ResetSession(ctx))
	assertNoSecrets(t, c.Close())
}

func TestStmt(t *testing.T) {
	ctx := context.Background()
	fc := &fullConn{query: func(string) (driver.Rows, error) { return &fullRows{}, nil }}
	s := &stmt{inner: &fullStmt{query: "q", conn: fc}}
	if s.NumInput() != -1 {
		t.Fatal("NumInput")
	}
	if _, err := s.Exec(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExecContext(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueryContext(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if s.Close() != nil {
		t.Fatal("close")
	}

	fc.execErr = errDial
	fc.query = func(string) (driver.Rows, error) { return nil, errLoginChain }
	_, err := s.Exec(nil)
	assertNoSecrets(t, err)
	_, err = s.QueryContext(ctx, nil)
	assertNoSecrets(t, err)
	assertNoSecrets(t, (&stmt{inner: &fullStmt{bareStmt: bareStmt{closeErr: errOddReply}}}).Close())

	br := &bareRows{closeErr: errors.New("ignored")}
	fc.query = func(string) (driver.Rows, error) { return br, nil }
	if _, err = s.Query(nil); !errors.Is(err, errUnsupportedDriverType) {
		t.Fatalf("got %v", err)
	}
}

func TestRows(t *testing.T) {
	r := &rows{inner: &fullRows{bareRows: bareRows{values: [][]driver.Value{{"x"}}}, nextSetErr: io.EOF}}
	dest := make([]driver.Value, 1)
	if r.Next(dest) != nil || dest[0] != "x" || r.Next(dest) != io.EOF || r.NextResultSet() != io.EOF { //nolint:errorlint // the bare sentinel is the contract
		t.Fatal("rows end")
	}
	if !reflect.DeepEqual(r.Columns(), []string{"a"}) || !r.HasNextResultSet() || r.Close() != nil {
		t.Fatal("delegation")
	}
	if r.ColumnTypeDatabaseTypeName(0) != "NVARCHAR" || r.ColumnTypeScanType(0) != reflect.TypeOf("") {
		t.Fatal("names")
	}
	if n, ok := r.ColumnTypeLength(0); n != 7 || !ok {
		t.Fatal("length")
	}
	if n, ok := r.ColumnTypeNullable(0); !n || !ok {
		t.Fatal("nullable")
	}
	if p, s, ok := r.ColumnTypePrecisionScale(0); p != 10 || s != 2 || !ok {
		t.Fatal("precision")
	}

	failing := &rows{inner: &fullRows{bareRows: bareRows{nextErr: errDial, closeErr: errOddReply}, nextSetErr: errLogin}}
	assertNoSecrets(t, failing.Next(dest))
	assertNoSecrets(t, failing.NextResultSet())
	assertNoSecrets(t, failing.Close())
}

func TestSanitize(t *testing.T) {
	isConn := func(e error) bool {
		var ce *ConnectionError
		return errors.As(e, &ce) && ce.Kind == KindConnection && ce.Op == "op"
	}
	cases := []struct {
		name string
		in   error
		want func(error) bool
	}{
		{"nil", nil, func(e error) bool { return e == nil }},
		{"skip", driver.ErrSkip, func(e error) bool { return e == driver.ErrSkip }},                                                    //nolint:errorlint // bare
		{"bad conn", errors.Join(errDial, driver.ErrBadConn), func(e error) bool { return e == driver.ErrBadConn }},                    //nolint:errorlint // bare
		{"canceled", errors.Join(errDial, context.Canceled), func(e error) bool { return e == context.Canceled }},                      //nolint:errorlint // bare
		{"deadline", errors.Join(errDial, context.DeadlineExceeded), func(e error) bool { return e == context.DeadlineExceeded }},      //nolint:errorlint // bare
		{"statement", errStatement, func(e error) bool { var se mssql.Error; return errors.As(e, &se) && se.Number == 208 }},           //nolint:errorlint // unchanged
		{"server error wrapping a statement", mssql.ServerError{}, func(e error) bool { return errors.As(e, new(mssql.ServerError)) }}, //nolint:errorlint // unchanged
		{"login", errLogin, isConn},
		{"login in the chain", errLoginChain, isConn},
		{"dial", errDial, isConn},
		{"errno", syscall.ECONNRESET, isConn},
		{"eof", io.ErrUnexpectedEOF, isConn},
		{"other", errOddReply, func(e error) bool {
			var de *DriverError
			return errors.As(e, &de) && de.Op == "op" && errors.Is(e, errOddReply) && e.Error() == "dalgo2mssql: op: the SQL Server driver failed"
		}},
	}
	for _, c := range cases {
		got := sanitize("op", c.in)
		if !c.want(got) {
			t.Errorf("%s: got %#v", c.name, got)
		}
		var se mssql.Error
		if got != nil && !errors.As(got, &se) {
			assertNoSecrets(t, got)
		}
	}
}

// TestDatabase_QuerySurfaceIsSanitised sends queries through the embedded
// dalgo2sql dal.DB, over the sanitising connector, and checks that no error on
// the way — the query, a later fetch, a transaction — carries a secret.
func TestDatabase_QuerySurfaceIsSanitised(t *testing.T) {
	ctx := context.Background()
	var queryErr error
	fc := &fullConn{}
	fc.query = func(q string) (driver.Rows, error) {
		switch q {
		case "SELECT SCHEMA_NAME()":
			return &fullRows{bareRows: bareRows{values: [][]driver.Value{{"dbo"}}}}, nil
		case "SELECT broken":
			return nil, queryErr
		case "SELECT midstream":
			return &fullRows{bareRows: bareRows{values: [][]driver.Value{{"x"}}, nextErr: errDial}}, nil
		}
		return nil, fmt.Errorf("unexpected query %q", q)
	}
	orig := newMSSQLConnector
	t.Cleanup(func() { newMSSQLConnector = orig })
	newMSSQLConnector = func(string) (driver.Connector, error) { return &fakeConnector{conn: fc}, nil }

	db, err := NewDatabase(ctx, secretDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	for _, cause := range []error{errDial, errLogin, errLoginChain, errOddReply} {
		queryErr = cause
		_, err = db.ExecuteQueryToRecordsReader(ctx, dal.NewTextQuery("SELECT broken", nil))
		assertNoSecrets(t, err)
		_, err = db.ExecuteQueryToRecordsetReader(ctx, dal.NewTextQuery("SELECT broken", nil))
		assertNoSecrets(t, err)
	}

	queryErr = errStatement
	_, err = db.ExecuteQueryToRecordsReader(ctx, dal.NewTextQuery("SELECT broken", nil))
	var se mssql.Error
	if !errors.As(err, &se) || se.Number != 208 {
		t.Errorf("a statement error must reach the caller: %v", err)
	}

	reader, err := db.ExecuteQueryToRecordsReader(ctx, dal.NewTextQuery("SELECT midstream", nil))
	if err != nil {
		t.Fatal(err)
	}
	_, err = reader.Next()
	if err != nil {
		t.Fatalf("first row: %v", err)
	}
	_, err = reader.Next()
	assertNoSecrets(t, err)
	_ = reader.Close()

	err = db.RunReadonlyTransaction(ctx, func(context.Context, dal.ReadTransaction) error { return nil })
	if !errors.Is(err, ErrReadOnlyTransaction) {
		t.Errorf("read-only transaction: %v", err)
	}
	fc.err = errLogin
	err = db.RunReadwriteTransaction(ctx, func(context.Context, dal.ReadwriteTransaction) error { return nil })
	assertNoSecrets(t, err)
}

// TestDatabase_PingThroughTheConnector checks that a failure to connect, met
// by NewDatabase's ping, carries no secret either way it nests.
func TestDatabase_PingThroughTheConnector(t *testing.T) {
	orig := newMSSQLConnector
	t.Cleanup(func() { newMSSQLConnector = orig })
	newMSSQLConnector = func(string) (driver.Connector, error) { return &fakeConnector{err: errLogin}, nil }
	_, err := NewDatabase(context.Background(), secretDSN)
	var connErr *ConnectionError
	if !errors.As(err, &connErr) || connErr.Kind != KindPing || !errors.As(err, new(mssql.Error)) {
		t.Fatalf("got %v", err)
	}
	assertNoSecrets(t, err)
}
