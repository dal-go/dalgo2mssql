package dalgo2mssql

import (
	"errors"
	"strings"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

func TestConnectionError_Text(t *testing.T) {
	cause := errors.New("cause")
	tests := []struct {
		err  *ConnectionError
		want string
	}{
		{&ConnectionError{Kind: KindOpen, cause: cause}, "dalgo2mssql: the connection string was refused by the SQL Server driver"},
		{&ConnectionError{Kind: KindConnection, Op: "ListIndexes", cause: cause}, "dalgo2mssql: ListIndexes: the connection to SQL Server failed"},
		{&ConnectionError{Kind: "other"}, "dalgo2mssql: the connection to SQL Server failed"},
	}
	for _, tt := range tests {
		if got := tt.err.Error(); got != tt.want {
			t.Errorf("got %q, want %q", got, tt.want)
		}
	}
	if !errors.Is(tests[0].err, cause) {
		t.Error("Unwrap")
	}
}

func TestClassify(t *testing.T) {
	statementErr := mssql.Error{Number: 229, Message: "The SELECT permission was denied on the object 'tables', database 'shopdb'"}
	err := classify("ListCollections", statementErr)
	var catalogErr *CatalogError
	if !errors.As(err, &catalogErr) || catalogErr.Number != 229 || catalogErr.Op != "ListCollections" {
		t.Fatalf("got %#v", err)
	}
	if err.Error() != "dalgo2mssql: ListCollections: catalog query failed (SQL Server error 229)" {
		t.Errorf("text %q", err.Error())
	}
	var asServer mssql.Error
	if !errors.As(err, &asServer) {
		t.Error("server error not reachable")
	}

	loginErr := mssql.Error{Number: 18456, Message: "Login failed for user 'reader_login'."}
	err = classify("ListIndexes", loginErr)
	var connErr *ConnectionError
	if !errors.As(err, &connErr) || connErr.Kind != KindConnection {
		t.Fatalf("got %#v", err)
	}
	if strings.Contains(err.Error(), "reader_login") {
		t.Errorf("text %q", err.Error())
	}

	plain := errors.New("read tcp db-host.example:1433: connection reset")
	err = classify("ListViews", plain)
	if !errors.As(err, &connErr) || !errors.Is(err, plain) || strings.Contains(err.Error(), "db-host") {
		t.Fatalf("got %v", err)
	}
}
