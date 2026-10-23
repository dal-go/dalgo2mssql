package dalgo2mssql

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
)

// secretDSN holds every part that must never reach an error's text.
const secretDSN = "sqlserver://reader_login:S3cretPassw0rd@db-host.example:1433?database=shopdb"

var secretParts = []string{"reader_login", "S3cretPassw0rd", "db-host.example", "shopdb", "sqlserver://"}

func assertNoSecrets(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, part := range secretParts {
		if strings.Contains(err.Error(), part) {
			t.Errorf("error %q contains %q", err.Error(), part)
		}
	}
}

// stubOpen makes sqlOpen return a sqlmock database that monitors pings.
func stubOpen(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true), sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	orig := sqlOpen
	sqlOpen = func(driverName, dsn string) (*sql.DB, error) {
		if driverName != "sqlserver" {
			t.Errorf("driver %q", driverName)
		}
		if dsn != secretDSN {
			t.Errorf("dsn %q", dsn)
		}
		return db, nil
	}
	t.Cleanup(func() {
		sqlOpen = orig
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
	return mock
}

func TestNewDatabase_DefaultSchema(t *testing.T) {
	mock := stubOpen(t)
	mock.ExpectPing()
	mock.ExpectQuery(`SELECT SCHEMA_NAME()`).WillReturnRows(sqlmock.NewRows([]string{"s"}).AddRow("dbo"))
	mock.ExpectClose()
	db, err := NewDatabase(context.Background(), secretDSN)
	if err != nil {
		t.Fatal(err)
	}
	if db.SchemaName() != "dbo" {
		t.Errorf("schema %q", db.SchemaName())
	}
	if a := db.Adapter(); a.Name() != "dalgo2mssql" || a.Version() != Version {
		t.Errorf("adapter %v %v", a.Name(), a.Version())
	}
	var _ dal.DB = db
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNewDatabase_NamedSchemaAndOptions(t *testing.T) {
	mock := stubOpen(t)
	mock.ExpectPing()
	mock.ExpectQuery(`SELECT s.name FROM sys.schemas s WHERE s.name = @p1`).WithArgs("sales").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("sales"))
	db, err := NewDatabase(context.Background(), secretDSN,
		WithSchema("sales"),
		WithDbOptions(dalgo2sql.DbOptions{ID: "shop", StructuredQueryDialect: "postgres"}),
		WithDalSchema(dal.NewSchema(nil, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if db.SchemaName() != "sales" {
		t.Errorf("schema %q", db.SchemaName())
	}
	if db.ID() != "shop" {
		t.Errorf("id %q", db.ID())
	}
}

func TestNewDatabase_SchemaNotFound(t *testing.T) {
	mock := stubOpen(t)
	mock.ExpectPing()
	mock.ExpectQuery(`SELECT s.name FROM sys.schemas s WHERE s.name = @p1`).WithArgs("nope").
		WillReturnRows(sqlmock.NewRows([]string{"name"}))
	mock.ExpectClose()
	_, err := NewDatabase(context.Background(), secretDSN, WithSchema("nope"))
	if !errors.Is(err, ErrSchemaNotFound) {
		t.Fatalf("got %v", err)
	}
	assertNoSecrets(t, err)
}

func TestNewDatabase_SchemaErrors(t *testing.T) {
	driverErr := errors.New("driver says " + secretDSN)
	tests := []struct {
		name   string
		opts   []Option
		expect func(sqlmock.Sqlmock)
		is     error
	}{
		{"named schema query fails", []Option{WithSchema("sales")}, func(m sqlmock.Sqlmock) {
			m.ExpectQuery(`SELECT s.name FROM sys.schemas s WHERE s.name = @p1`).WillReturnError(driverErr)
		}, driverErr},
		{"default schema query fails", nil, func(m sqlmock.Sqlmock) {
			m.ExpectQuery(`SELECT SCHEMA_NAME()`).WillReturnError(driverErr)
		}, driverErr},
		{"default schema is NULL", nil, func(m sqlmock.Sqlmock) {
			m.ExpectQuery(`SELECT SCHEMA_NAME()`).WillReturnRows(sqlmock.NewRows([]string{"s"}).AddRow(nil))
		}, ErrSchemaNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := stubOpen(t)
			mock.ExpectPing()
			tt.expect(mock)
			mock.ExpectClose()
			_, err := NewDatabase(context.Background(), secretDSN, tt.opts...)
			var connErr *ConnectionError
			if !errors.As(err, &connErr) || connErr.Kind != KindSchema {
				t.Fatalf("got %v", err)
			}
			if !errors.Is(err, tt.is) {
				t.Errorf("cause not reachable: %v", err)
			}
			assertNoSecrets(t, err)
		})
	}
}

func TestNewDatabase_PingFails(t *testing.T) {
	mock := stubOpen(t)
	pingErr := errors.New("dial tcp db-host.example:1433: login reader_login S3cretPassw0rd")
	mock.ExpectPing().WillReturnError(pingErr)
	mock.ExpectClose()
	_, err := NewDatabase(context.Background(), secretDSN)
	var connErr *ConnectionError
	if !errors.As(err, &connErr) || connErr.Kind != KindPing {
		t.Fatalf("got %v", err)
	}
	if !errors.Is(err, pingErr) {
		t.Error("driver error not reachable through Unwrap")
	}
	assertNoSecrets(t, err)
}

func TestNewDatabase_OpenFails(t *testing.T) {
	orig := sqlOpen
	t.Cleanup(func() { sqlOpen = orig })
	openErr := errors.New("bad dsn " + secretDSN)
	sqlOpen = func(string, string) (*sql.DB, error) { return nil, openErr }
	_, err := NewDatabase(context.Background(), secretDSN)
	var connErr *ConnectionError
	if !errors.As(err, &connErr) || connErr.Kind != KindOpen || !errors.Is(err, openErr) {
		t.Fatalf("got %v", err)
	}
	assertNoSecrets(t, err)
}

// TestNewDatabase_RealDriverFailures goes through the real go-mssqldb driver:
// a DSN it refuses, and a server that is not there (a closed loopback port).
func TestNewDatabase_RealDriverFailures(t *testing.T) {
	t.Run("refused dsn", func(t *testing.T) {
		_, err := NewDatabase(context.Background(), "sqlserver://reader_login:S3cretPassw0rd@db-host.example:notaport?database=shopdb")
		var connErr *ConnectionError
		if !errors.As(err, &connErr) || connErr.Kind != KindOpen {
			t.Fatalf("got %v", err)
		}
		assertNoSecrets(t, err)
	})
	t.Run("unreachable server", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := NewDatabase(ctx, "sqlserver://reader_login:S3cretPassw0rd@127.0.0.1:1?database=shopdb&dial+timeout=2")
		var connErr *ConnectionError
		if !errors.As(err, &connErr) || connErr.Kind != KindPing {
			t.Fatalf("got %v", err)
		}
		if errors.Unwrap(err) == nil {
			t.Error("driver error not reachable")
		}
		assertNoSecrets(t, err)
	})
}
