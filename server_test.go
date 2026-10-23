package dalgo2mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

// serverURLEnv names the admin URL of a real SQL Server
// (sqlserver://sa:pw@127.0.0.1:port?database=master). The test is skipped
// when it is not set.
const serverURLEnv = "DALGO2MSSQL_TEST_URL"

var serverSetup = []string{
	`CREATE SCHEMA ext`,
	`CREATE TABLE ext.tenants (
		tenant_id int NOT NULL,
		region nvarchar(10) NOT NULL,
		CONSTRAINT pk_tenants PRIMARY KEY (tenant_id, region))`,
	`CREATE TABLE dbo.shops (
		id int IDENTITY(1,1) NOT NULL CONSTRAINT pk_shops PRIMARY KEY,
		name nvarchar(100) NOT NULL CONSTRAINT uq_shops_name UNIQUE,
		created datetime2(3) NOT NULL CONSTRAINT df_shops_created DEFAULT (getdate()),
		rating decimal(5,2) NULL CONSTRAINT df_shops_rating DEFAULT 0,
		tenant_id int NULL,
		region nvarchar(10) NULL,
		CONSTRAINT ck_shops_rating CHECK (rating >= 0),
		CONSTRAINT fk_shops_tenant FOREIGN KEY (tenant_id, region) REFERENCES ext.tenants (tenant_id, region))`,
	`CREATE TABLE dbo.orders (
		shop_id int NOT NULL,
		order_no int NOT NULL,
		note varchar(max) NULL,
		alt_shop_id int NULL,
		CONSTRAINT pk_orders PRIMARY KEY (order_no, shop_id),
		CONSTRAINT fk_orders_shop FOREIGN KEY (shop_id) REFERENCES dbo.shops (id) ON DELETE CASCADE,
		CONSTRAINT fk_orders_alt FOREIGN KEY (alt_shop_id) REFERENCES dbo.shops (id))`,
	`ALTER TABLE dbo.orders NOCHECK CONSTRAINT fk_orders_alt`,
	`CREATE INDEX ix_shops_created ON dbo.shops (created DESC, rating) INCLUDE (name)`,
	`CREATE VIEW dbo.v_shops AS SELECT id, name FROM dbo.shops`,
}

func withDatabase(t *testing.T, raw, database string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", serverURLEnv, err)
	}
	q := u.Query()
	q.Set("database", database)
	u.RawQuery = q.Encode()
	return u.String()
}

func TestServer(t *testing.T) {
	adminURL := os.Getenv(serverURLEnv)
	if adminURL == "" {
		t.Skip(serverURLEnv + " is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	admin, err := sql.Open(DriverName, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	// A cleanup, not a defer: cleanups run after the test function returns
	// and in reverse order, so the pool is still open when the cleanup below
	// drops the database.
	t.Cleanup(func() { _ = admin.Close() })
	dbName := fmt.Sprintf("dalgo2mssql_%d", time.Now().UnixNano())
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE ["+dbName+"]"); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), time.Minute)
		defer dropCancel()
		if _, err := admin.ExecContext(dropCtx, "ALTER DATABASE ["+dbName+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE ["+dbName+"]"); err != nil {
			t.Errorf("drop database: %v", err)
		}
	})
	dsn := withDatabase(t, adminURL, dbName)
	setup, err := sql.Open(DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range serverSetup {
		if _, err = setup.ExecContext(ctx, stmt); err != nil {
			_ = setup.Close()
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	_ = setup.Close()

	db, err := NewDatabase(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if db.SchemaName() != "dbo" {
		t.Errorf("schema %q", db.SchemaName())
	}

	t.Run("ListCollections", func(t *testing.T) {
		refs, err := db.ListCollections(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := names(refs); !reflect.DeepEqual(got, []string{"orders", "shops", "v_shops"}) {
			t.Errorf("got %v", got)
		}
		views, err := db.ListViews(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := names(views); !reflect.DeepEqual(got, []string{"v_shops"}) {
			t.Errorf("views %v", got)
		}
	})

	t.Run("DescribeCollection", func(t *testing.T) {
		def, err := db.DescribeCollection(ctx, ref("shops"))
		if err != nil {
			t.Fatal(err)
		}
		wantFields := []dbschema.FieldDef{
			{Name: "id", Type: dbschema.Int, AutoIncrement: true},
			{Name: "name", Type: dbschema.String, Length: intPtr(100)},
			{Name: "created", Type: dbschema.Time, Default: dbschema.DefaultLiteral{Value: "getdate()"}},
			{Name: "rating", Type: dbschema.Decimal, Precision: &dbschema.Precision{Total: 5, Scale: 2}, Nullable: true, Default: dbschema.DefaultLiteral{Value: "0"}},
			{Name: "tenant_id", Type: dbschema.Int, Nullable: true},
			{Name: "region", Type: dbschema.String, Length: intPtr(10), Nullable: true},
		}
		if !reflect.DeepEqual(def.Fields, wantFields) {
			t.Errorf("fields\n got %+v\nwant %+v", def.Fields, wantFields)
		}
		if !reflect.DeepEqual(def.PrimaryKey, []dal.FieldName{"id"}) {
			t.Errorf("pk %v", def.PrimaryKey)
		}
		wantFK := []dbschema.ForeignKeyDef{{
			Name: "fk_shops_tenant", Fields: []dal.FieldName{"tenant_id", "region"},
			ReferencedCollection: "tenants", ReferencedNamespace: "ext", ReferencedFields: []dal.FieldName{"tenant_id", "region"},
			Enforcement: dbschema.ForeignKeyEnforcementEnabled, OnUpdate: "NO ACTION", OnDelete: "NO ACTION",
		}}
		if !reflect.DeepEqual(def.ForeignKeys, wantFK) {
			t.Errorf("fks %+v", def.ForeignKeys)
		}

		orders, err := db.DescribeCollection(ctx, ref("orders"))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(orders.PrimaryKey, []dal.FieldName{"order_no", "shop_id"}) {
			t.Errorf("orders pk %v", orders.PrimaryKey)
		}
		if note := orders.Fields[2]; note.Name != "note" || note.Type != dbschema.String || note.Length != nil || !note.Nullable {
			t.Errorf("note %+v", note)
		}
		byName := map[string]dbschema.ForeignKeyDef{}
		for _, key := range orders.ForeignKeys {
			byName[key.Name] = key
		}
		if k := byName["fk_orders_shop"]; k.ReferencedCollection != "shops" || k.ReferencedNamespace != "" || k.OnDelete != "CASCADE" || k.Enforcement != dbschema.ForeignKeyEnforcementEnabled {
			t.Errorf("fk_orders_shop %+v", k)
		}
		if k := byName["fk_orders_alt"]; k.Enforcement != dbschema.ForeignKeyEnforcementDisabled {
			t.Errorf("fk_orders_alt %+v", k)
		}

		view, err := db.DescribeCollection(ctx, ref("v_shops"))
		if err != nil {
			t.Fatal(err)
		}
		if len(view.Fields) != 2 || len(view.PrimaryKey) != 0 || len(view.ForeignKeys) != 0 {
			t.Errorf("view %+v", view)
		}

		tenants, err := db.DescribeCollection(ctx, func() *dal.CollectionRef {
			r := dal.NewQualifiedRootCollectionRef("ext", "tenants", "")
			return &r
		}())
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(tenants.PrimaryKey, []dal.FieldName{"tenant_id", "region"}) {
			t.Errorf("tenants pk %v", tenants.PrimaryKey)
		}

		if _, err = db.DescribeCollection(ctx, ref("missing")); !errors.Is(err, ErrCollectionNotFound) {
			t.Errorf("missing: %v", err)
		}
	})

	t.Run("ListIndexes", func(t *testing.T) {
		got, err := db.ListIndexes(ctx, ref("shops"))
		if err != nil {
			t.Fatal(err)
		}
		want := []dbschema.IndexDef{
			{Name: "ix_shops_created", Collection: "shops", Fields: []dal.FieldName{"created", "rating"}},
			{Name: "uq_shops_name", Collection: "shops", Fields: []dal.FieldName{"name"}, Unique: true},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("ListConstraints", func(t *testing.T) {
		got, err := db.ListConstraints(ctx, ref("shops"))
		if err != nil {
			t.Fatal(err)
		}
		want := []dbschema.ConstraintDef{
			{Name: "ck_shops_rating", Type: "check"},
			{Name: "fk_shops_tenant", Type: "foreign-key"},
			{Name: "pk_shops", Type: "primary-key"},
			{Name: "uq_shops_name", Type: "unique"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("ListReferrers", func(t *testing.T) {
		got, err := db.ListReferrers(ctx, ref("shops"))
		if err != nil {
			t.Fatal(err)
		}
		want := []dbschema.Referrer{
			{Collection: dal.NewRootCollectionRef("orders", ""), Fields: []dal.FieldName{"alt_shop_id"}},
			{Collection: dal.NewRootCollectionRef("orders", ""), Fields: []dal.FieldName{"shop_id"}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("WithSchema", func(t *testing.T) {
		ext, err := NewDatabase(ctx, dsn, WithSchema("ext"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ext.Close() }()
		refs, err := ext.ListCollections(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := names(refs); !reflect.DeepEqual(got, []string{"tenants"}) {
			t.Errorf("got %v", got)
		}
		if _, err = NewDatabase(ctx, dsn, WithSchema("nope")); !errors.Is(err, ErrSchemaNotFound) {
			t.Errorf("nope: %v", err)
		}
	})

	t.Run("bad password leaks nothing", func(t *testing.T) {
		u, _ := url.Parse(dsn)
		const badPassword = "Wr0ng-Passw0rd-Secret"
		u.User = url.UserPassword(u.User.Username(), badPassword)
		_, err := NewDatabase(ctx, u.String())
		var connErr *ConnectionError
		if !errors.As(err, &connErr) || connErr.Kind != KindPing {
			t.Fatalf("got %v", err)
		}
		for _, part := range []string{badPassword, u.User.Username(), u.Hostname(), dbName} {
			if strings.Contains(err.Error(), part) {
				t.Errorf("error %q contains %q", err.Error(), part)
			}
		}
	})
}
