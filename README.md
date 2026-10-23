# dalgo2mssql

Microsoft SQL Server driver of DALgo. Wraps `github.com/dal-go/dalgo2sql` to
provide the `dal.DB` surface, and adds a SQL Server-native implementation of:

- `dbschema.SchemaReader` — schema introspection through the `sys.*` catalog
  views, scoped to one SQL Server schema (the login's default schema,
  `SCHEMA_NAME()`, usually `dbo`, unless `WithSchema` names another)
- `ListViews` — the views of the schema, so a schema browser can tell a view
  from a table
- `io.Closer`

The driver is [`github.com/microsoft/go-mssqldb`](https://github.com/microsoft/go-mssqldb)
(driver name `"sqlserver"`), pure Go: the package compiles with `CGO_ENABLED=0`.

## Constructor usage

```go
import (
    "context"

    "github.com/dal-go/dalgo2mssql"
)

db, err := dalgo2mssql.NewDatabase(ctx,
    "sqlserver://reader:secret@127.0.0.1:1433?database=shop")
if err != nil {
    return err
}
defer db.Close()

refs, err := db.ListCollections(ctx, nil)          // tables and views of dbo
def, err := db.DescribeCollection(ctx, &refs[0])   // columns, keys, indexes
```

`NewDatabase` opens the server, pings it with `ctx` and resolves the schema to
read; a failure at any of those steps closes the pool again.

### Options

| Option | Effect |
|--------|--------|
| `WithSchema(name)` | Read schema `name` instead of the login's default schema. A schema that does not exist fails `NewDatabase` with an error that matches `ErrSchemaNotFound`. |
| `WithDbOptions(dalgo2sql.DbOptions)` | Options of the embedded `dalgo2sql` database (recordsets, …). `StructuredQueryDialect` is always replaced with `"sqlserver"`. |
| `WithDalSchema(dal.Schema)` | The `dal.Schema` of the embedded database (an empty one by default). |

A collection reference that names a schema of its own
(`dal.NewQualifiedRootCollectionRef("ext", "tenants", "")`) is read in that
schema, whatever the `Database` was opened with.

## What the schema reader reports

| Method | Reports |
|--------|---------|
| `ListCollections` | User tables and views of the schema, by name. Objects SQL Server ships (`is_ms_shipped`) are left out. |
| `ListViews` | The views of the schema, by name (each is also in `ListCollections`). |
| `DescribeCollection` | Columns (portable type, length, precision, nullability, default expression, identity), the primary key in key order, the indexes and the foreign keys. |
| `ListIndexes` | Indexes other than the primary key, key columns in key order; included columns are left out. The index behind a `UNIQUE` constraint is listed under the constraint's name, unique. |
| `ListConstraints` | `PRIMARY KEY`, `UNIQUE`, `FOREIGN KEY` and `CHECK` constraints (types `primary-key`, `unique`, `foreign-key`, `check`), by name within type. |
| `ListReferrers` | One entry per foreign key that references the table, from tables of the same schema, with the referencing columns in key order. |

Details:

- A default definition loses the parentheses SQL Server wraps it in: `((0))` is
  `0`, `(getdate())` is `getdate()`.
- A primary-key column is never reported nullable.
- A foreign key into another schema names it in `ReferencedNamespace`; one
  disabled with `NOCHECK` is reported with `ForeignKeyEnforcementDisabled`.
  `OnUpdate` / `OnDelete` are `NO ACTION`, `CASCADE`, `SET NULL` or
  `SET DEFAULT`.
- A missing table or view is an error that matches `ErrCollectionNotFound`.

## Type mapping

| SQL Server type | `dbschema.Type` | Length / precision |
|-----------------|-----------------|--------------------|
| `bit` | `Bool` | |
| `tinyint`, `smallint`, `int`, `bigint` | `Int` | |
| `real`, `float` | `Float` | |
| `decimal`, `numeric`, `money`, `smallmoney` | `Decimal` | `Precision{Total, Scale}` |
| `char`, `varchar` | `String` | `Length` in characters; none for `MAX` |
| `nchar`, `nvarchar` | `String` | `Length` in characters (bytes / 2); none for `MAX` |
| `binary`, `varbinary` | `Bytes` | `Length`; none for `MAX` |
| `image`, `timestamp`/`rowversion`, `hierarchyid`, `geography`, `geometry` | `Bytes` | |
| `date`, `time`, `datetime`, `datetime2`, `smalldatetime`, `datetimeoffset` | `Time` | |
| anything else (`text`, `ntext`, `xml`, `uniqueidentifier`, `sql_variant`, …) | `String` | |

An alias type is mapped by its system type.

## Errors are sanitised

No error of this package carries the DSN, the password, the user, the host or
the database name, and none carries the driver's or the server's message text,
which can hold them:

- `*ConnectionError` — a failure to open (`KindOpen`), ping (`KindPing`) or
  resolve the schema (`KindSchema`), or a connection lost during a later call
  (`KindConnection`, with `Op` naming the call). Login errors the server
  returns on a later call (18456 and the like) are connection errors too.
- `*CatalogError` — a catalog query the server rejected (for example for lack
  of `VIEW DEFINITION`); its text holds the call and the SQL Server error
  number only.

Each has fixed text; the driver's error stays reachable through `Unwrap` for
`errors.Is` and `errors.As`.

## Read-only access: use a read-only login

SQL Server has no read-only session and no read-only transaction, and this
package does not pretend to have one: nothing here stops a statement sent
through the embedded `dal.DB` from writing. For read-only use, connect as a
login that can only read, for example a database user that is only a member of
`db_datareader` and has `VIEW DEFINITION` for the schema scan:

```sql
CREATE LOGIN reader WITH PASSWORD = '...';
USE shop;
CREATE USER reader FOR LOGIN reader;
ALTER ROLE db_datareader ADD MEMBER reader;
GRANT VIEW DEFINITION ON SCHEMA::dbo TO reader;
```

## Current limitations

- **Structured reads need dalgo2sql's `"sqlserver"` dialect.** The package
  sets `StructuredQueryDialect = "sqlserver"`, which compiles to `@p1`, `@p2`,
  … parameters. The dialect is being added to `dal-go/dalgo2sql`; until a
  release has it, a structured read is refused with dalgo2sql's
  `unsupported structured query dialect "sqlserver"` error. The schema reader
  does not depend on it.
- **Key reads and writes do not work yet.** `Get`, `Set`, `Insert` and
  `Delete` of the embedded `dal.DB` write `?` parameter markers, which the
  `sqlserver` driver does not accept; dalgo2sql has no `@p1` placeholder style
  yet.
- No `ddl.Applier`: the package does not create or alter tables.

## Running the tests

The unit tests use [go-sqlmock](https://github.com/DATA-DOG/go-sqlmock) and
cover every statement. `TestServer` runs against a real SQL Server and is
skipped unless `DALGO2MSSQL_TEST_URL` holds an admin URL; it creates a database
of its own and drops it at the end.

```sh
docker run -d --name mssql2022 \
  -e ACCEPT_EULA=Y -e MSSQL_SA_PASSWORD='Dalgo2mssql-Test1' \
  -p 1433:1433 mcr.microsoft.com/mssql/server:2022-latest

DALGO2MSSQL_TEST_URL='sqlserver://sa:Dalgo2mssql-Test1@127.0.0.1:1433?database=master&TrustServerCertificate=true' \
    go test ./... -count=1 -v
```

The image is SQL Server Developer edition; `ACCEPT_EULA=Y` accepts its licence,
which allows development and testing only. CI runs `TestServer` on every pull
request and fails if it is skipped.
