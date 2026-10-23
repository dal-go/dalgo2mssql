// Package dalgo2mssql is the Microsoft SQL Server adapter of DALgo: it reads a
// SQL Server schema and runs structured queries.
//
// [NewDatabase] opens the server with github.com/microsoft/go-mssqldb (driver
// name "sqlserver", DSN form sqlserver://user:pass@host:1433?database=shop),
// pings it, and wraps the *sql.DB with [dalgo2sql.NewDatabase] under the
// "sqlserver" structured-query dialect, so a [*Database] is a [dal.DB]. It also
// is a [dbschema.SchemaReader] and an [io.Closer], and it lists views
// (ListViews), which lets a schema browser tell a view from a table.
//
// # Scope
//
// A Database reads one schema: the login's default schema (SCHEMA_NAME(),
// usually dbo) unless [WithSchema] names another. It reads user tables and
// views; objects SQL Server ships (is_ms_shipped) are left out. A collection
// reference that names a schema of its own (dal.NewQualifiedRootCollectionRef)
// is read in that schema.
//
// # Read-only
//
// SQL Server has no read-only session and no read-only transaction, and this
// package does not pretend to have one: nothing here stops a statement sent
// through the embedded dal.DB from writing, and a transaction begun read-only
// (RunReadonlyTransaction) fails with [ErrReadOnlyTransaction]. Safety comes
// from outside this package: the caller sends only SELECT text, and the login
// in the DSN should be a read-only one (for example a user that is only a
// member of db_datareader and has VIEW DEFINITION for the schema scan).
//
// The key read/write surface of the embedded dal.DB (Get, Set, Insert, Delete)
// writes "?" parameter markers, which the "sqlserver" driver does not accept;
// structured reads compiled by dalgo2sql's "sqlserver" dialect use @p1, @p2, …
// and are the supported way to read data. A dalgo2sql release older than the
// one that adds the "sqlserver" dialect refuses structured reads with its
// unsupported-dialect error; the schema reader does not depend on it.
//
// # Errors
//
// No error of this package carries the DSN, the password, the user or the
// host. The pool is opened over [NewConnector], which sends every error of the
// driver through one function, so this holds for the query, read and
// transaction methods of the embedded dal.DB too. A failure to open, ping or
// resolve the schema is a [*ConnectionError]; so is a failed reconnect, a lost
// connection or a login or database error on a later call. A catalog query the
// server rejects is a [*CatalogError] with the server's error number; another
// error of the driver is a [*DriverError]. Each has fixed text, and the
// driver's error stays reachable through Unwrap for errors.Is and errors.As.
// An error the server returns for a statement the caller sent is go-mssqldb's
// mssql.Error, unchanged: its message describes the statement and can name
// its objects, including their database.
package dalgo2mssql
