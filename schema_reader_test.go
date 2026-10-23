package dalgo2mssql

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

func newReader(t *testing.T) (*Database, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		_ = db.Close()
	})
	return &Database{sqlDB: db, schema: "dbo"}, mock
}

var errDriver = errors.New("driver failure")

func ref(name string) *dal.CollectionRef {
	r := dal.NewRootCollectionRef(name, "")
	return &r
}

func expectLookup(mock sqlmock.Sqlmock, schema, name string, id int64) {
	mock.ExpectQuery(lookupObjectSQL).WithArgs(schema, name).
		WillReturnRows(sqlmock.NewRows([]string{"object_id"}).AddRow(id))
}

func names(refs []dal.CollectionRef) []string {
	var out []string
	for _, r := range refs {
		out = append(out, r.Name())
	}
	return out
}

func TestListCollectionsAndViews(t *testing.T) {
	d, mock := newReader(t)
	mock.ExpectQuery(listObjectsSQL).WithArgs("dbo").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("customers").AddRow("orders").AddRow("v_orders"))
	mock.ExpectQuery(listViewsSQL).WithArgs("dbo").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("v_orders"))
	refs, err := d.ListCollections(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(refs); !reflect.DeepEqual(got, []string{"customers", "orders", "v_orders"}) {
		t.Errorf("collections %v", got)
	}
	views, err := d.ListViews(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := names(views); !reflect.DeepEqual(got, []string{"v_orders"}) {
		t.Errorf("views %v", got)
	}
}

func TestListCollections_Errors(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		d, mock := newReader(t)
		mock.ExpectQuery(listObjectsSQL).WillReturnError(errDriver)
		if _, err := d.ListCollections(context.Background(), nil); !errors.Is(err, errDriver) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("scan", func(t *testing.T) {
		d, mock := newReader(t)
		mock.ExpectQuery(listViewsSQL).WillReturnRows(sqlmock.NewRows([]string{"name", "extra"}).AddRow("a", "b"))
		if _, err := d.ListViews(context.Background()); err == nil {
			t.Fatal("expected scan error")
		}
	})
	t.Run("rows", func(t *testing.T) {
		d, mock := newReader(t)
		mock.ExpectQuery(listObjectsSQL).WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("a").RowError(0, errDriver))
		if _, err := d.ListCollections(context.Background(), nil); !errors.Is(err, errDriver) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestLookup(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		d, mock := newReader(t)
		mock.ExpectQuery(lookupObjectSQL).WithArgs("sales", "missing").WillReturnRows(sqlmock.NewRows([]string{"object_id"}))
		r := dal.NewQualifiedRootCollectionRef("sales", "missing", "")
		_, err := d.ListIndexes(context.Background(), &r)
		if !errors.Is(err, ErrCollectionNotFound) {
			t.Fatalf("got %v", err)
		}
		if err.Error() != "dalgo2mssql: collection not found: sales.missing" {
			t.Errorf("text %q", err.Error())
		}
	})
	t.Run("error", func(t *testing.T) {
		d, mock := newReader(t)
		mock.ExpectQuery(lookupObjectSQL).WillReturnError(errDriver)
		var connErr *ConnectionError
		if _, err := d.DescribeCollection(context.Background(), ref("t")); !errors.As(err, &connErr) || connErr.Op != "DescribeCollection" {
			t.Fatalf("got %v", err)
		}
	})
	for name, call := range map[string]func(*Database) error{
		"ListConstraints": func(d *Database) error { _, err := d.ListConstraints(context.Background(), ref("t")); return err },
		"ListReferrers":   func(d *Database) error { _, err := d.ListReferrers(context.Background(), ref("t")); return err },
	} {
		t.Run(name, func(t *testing.T) {
			d, mock := newReader(t)
			mock.ExpectQuery(lookupObjectSQL).WillReturnRows(sqlmock.NewRows([]string{"object_id"}))
			if err := call(d); !errors.Is(err, ErrCollectionNotFound) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func expectDescribe(mock sqlmock.Sqlmock) {
	expectLookup(mock, "dbo", "orders", 42)
	mock.ExpectQuery(primaryKeySQL).WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("shop_id").AddRow("order_id"))
	mock.ExpectQuery(columnsSQL).WithArgs(int64(42)).WillReturnRows(
		sqlmock.NewRows([]string{"name", "type", "max_length", "precision", "scale", "is_nullable", "is_identity", "definition"}).
			AddRow("order_id", "int", 4, 10, 0, false, true, nil).
			AddRow("shop_id", "int", 4, 10, 0, true, false, nil).
			AddRow("title", "nvarchar", 200, 0, 0, true, false, "(N'none')").
			AddRow("total", "decimal", 9, 10, 2, false, false, "((0))").
			AddRow("created", "datetime2", 8, 27, 7, false, false, "(getdate())"))
	mock.ExpectQuery(indexesSQL).WithArgs(int64(42)).WillReturnRows(
		sqlmock.NewRows([]string{"name", "is_unique", "column"}).
			AddRow("ix_orders_shop", false, "shop_id").
			AddRow("ix_orders_shop", false, "created").
			AddRow("uq_orders_title", true, "title"))
	mock.ExpectQuery(foreignKeysSQL).WithArgs(int64(42)).WillReturnRows(
		sqlmock.NewRows([]string{"name", "from", "rs", "rt", "to", "upd", "del", "disabled"}).
			AddRow("fk_orders_shop", "shop_id", "dbo", "shops", "id", "NO_ACTION", "CASCADE", false).
			AddRow("fk_orders_ext", "shop_id", "ext", "tenants", "tenant_shop", "SET_NULL", "SET_DEFAULT", true).
			AddRow("fk_orders_ext", "order_id", "ext", "tenants", "tenant_order", "SET_NULL", "SET_DEFAULT", true))
}

func intPtr(n int) *int { return &n }

func TestDescribeCollection(t *testing.T) {
	d, mock := newReader(t)
	expectDescribe(mock)
	def, err := d.DescribeCollection(context.Background(), ref("orders"))
	if err != nil {
		t.Fatal(err)
	}
	want := &dbschema.CollectionDef{
		Name: "orders",
		Fields: []dbschema.FieldDef{
			{Name: "order_id", Type: dbschema.Int, AutoIncrement: true},
			{Name: "shop_id", Type: dbschema.Int},
			{Name: "title", Type: dbschema.String, Length: intPtr(100), Nullable: true, Default: dbschema.DefaultLiteral{Value: "N'none'"}},
			{Name: "total", Type: dbschema.Decimal, Precision: &dbschema.Precision{Total: 10, Scale: 2}, Default: dbschema.DefaultLiteral{Value: "0"}},
			{Name: "created", Type: dbschema.Time, Default: dbschema.DefaultLiteral{Value: "getdate()"}},
		},
		PrimaryKey: []dal.FieldName{"shop_id", "order_id"},
		Indexes: []dbschema.IndexDef{
			{Name: "ix_orders_shop", Collection: "orders", Fields: []dal.FieldName{"shop_id", "created"}},
			{Name: "uq_orders_title", Collection: "orders", Fields: []dal.FieldName{"title"}, Unique: true},
		},
		ForeignKeys: []dbschema.ForeignKeyDef{
			{Name: "fk_orders_shop", Fields: []dal.FieldName{"shop_id"}, ReferencedCollection: "shops", ReferencedFields: []dal.FieldName{"id"},
				Enforcement: dbschema.ForeignKeyEnforcementEnabled, OnUpdate: "NO ACTION", OnDelete: "CASCADE"},
			{Name: "fk_orders_ext", Fields: []dal.FieldName{"shop_id", "order_id"}, ReferencedCollection: "tenants", ReferencedNamespace: "ext",
				ReferencedFields: []dal.FieldName{"tenant_shop", "tenant_order"},
				Enforcement:      dbschema.ForeignKeyEnforcementDisabled, OnUpdate: "SET NULL", OnDelete: "SET DEFAULT"},
		},
	}
	if !reflect.DeepEqual(def, want) {
		t.Errorf("got  %+v\nwant %+v", def, want)
	}
}

// TestDescribeCollection_StageErrors fails each catalog query of
// DescribeCollection in turn, by a query error and by a scan error.
func TestDescribeCollection_StageErrors(t *testing.T) {
	queries := []string{primaryKeySQL, columnsSQL, indexesSQL, foreignKeysSQL}
	ok := map[string]*sqlmock.Rows{}
	resetOK := func() {
		ok[primaryKeySQL] = sqlmock.NewRows([]string{"name"})
		ok[columnsSQL] = sqlmock.NewRows([]string{"name", "type", "max_length", "precision", "scale", "is_nullable", "is_identity", "definition"})
		ok[indexesSQL] = sqlmock.NewRows([]string{"name", "is_unique", "column"})
		ok[foreignKeysSQL] = sqlmock.NewRows([]string{"name", "from", "rs", "rt", "to", "upd", "del", "disabled"})
	}
	for failAt := range queries {
		for _, mode := range []string{"query", "scan"} {
			t.Run(queries[failAt][:30]+"/"+mode, func(t *testing.T) {
				resetOK()
				d, mock := newReader(t)
				expectLookup(mock, "dbo", "t", 7)
				for i := 0; i < failAt; i++ {
					mock.ExpectQuery(queries[i]).WillReturnRows(ok[queries[i]])
				}
				if mode == "query" {
					mock.ExpectQuery(queries[failAt]).WillReturnError(errDriver)
				} else {
					mock.ExpectQuery(queries[failAt]).WillReturnRows(sqlmock.NewRows([]string{"a", "b"}).AddRow("x", "y"))
				}
				if _, err := d.DescribeCollection(context.Background(), ref("t")); err == nil {
					t.Fatal("expected an error")
				}
			})
		}
	}
}

func TestListIndexes(t *testing.T) {
	d, mock := newReader(t)
	expectLookup(mock, "dbo", "orders", 42)
	mock.ExpectQuery(indexesSQL).WithArgs(int64(42)).WillReturnRows(
		sqlmock.NewRows([]string{"name", "is_unique", "column"}).AddRow("uq_title", true, "title"))
	got, err := d.ListIndexes(context.Background(), ref("orders"))
	if err != nil {
		t.Fatal(err)
	}
	want := []dbschema.IndexDef{{Name: "uq_title", Collection: "orders", Fields: []dal.FieldName{"title"}, Unique: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}

	d, mock = newReader(t)
	expectLookup(mock, "dbo", "orders", 42)
	mock.ExpectQuery(indexesSQL).WillReturnError(errDriver)
	if _, err = d.ListIndexes(context.Background(), ref("orders")); !errors.Is(err, errDriver) {
		t.Fatalf("got %v", err)
	}
}

func TestListConstraints(t *testing.T) {
	d, mock := newReader(t)
	expectLookup(mock, "dbo", "orders", 42)
	mock.ExpectQuery(constraintsSQL).WithArgs(int64(42)).WillReturnRows(
		sqlmock.NewRows([]string{"name", "type"}).
			AddRow("ck_total", "C").AddRow("fk_shop", "F").AddRow("pk_orders", "PK").AddRow("uq_title", "UQ"))
	got, err := d.ListConstraints(context.Background(), ref("orders"))
	if err != nil {
		t.Fatal(err)
	}
	want := []dbschema.ConstraintDef{
		{Name: "ck_total", Type: "check"}, {Name: "fk_shop", Type: "foreign-key"},
		{Name: "pk_orders", Type: "primary-key"}, {Name: "uq_title", Type: "unique"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}

	d, mock = newReader(t)
	expectLookup(mock, "dbo", "orders", 42)
	mock.ExpectQuery(constraintsSQL).WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("x"))
	if _, err = d.ListConstraints(context.Background(), ref("orders")); err == nil {
		t.Fatal("expected scan error")
	}
}

func TestListReferrers(t *testing.T) {
	d, mock := newReader(t)
	expectLookup(mock, "dbo", "shops", 9)
	mock.ExpectQuery(referrersSQL).WithArgs(int64(9), "dbo").WillReturnRows(
		sqlmock.NewRows([]string{"table", "key", "column"}).
			AddRow("orders", "fk_a", "shop_id").
			AddRow("orders", "fk_b", "alt_shop_id").
			AddRow("stock", "fk_stock", "shop_id").
			AddRow("stock", "fk_stock", "region_id"))
	got, err := d.ListReferrers(context.Background(), ref("shops"))
	if err != nil {
		t.Fatal(err)
	}
	want := []dbschema.Referrer{
		{Collection: dal.NewRootCollectionRef("orders", ""), Fields: []dal.FieldName{"shop_id"}},
		{Collection: dal.NewRootCollectionRef("orders", ""), Fields: []dal.FieldName{"alt_shop_id"}},
		{Collection: dal.NewRootCollectionRef("stock", ""), Fields: []dal.FieldName{"shop_id", "region_id"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}

	d, mock = newReader(t)
	expectLookup(mock, "dbo", "shops", 9)
	mock.ExpectQuery(referrersSQL).WillReturnRows(sqlmock.NewRows([]string{"table"}).AddRow("x"))
	if _, err = d.ListReferrers(context.Background(), ref("shops")); err == nil {
		t.Fatal("expected scan error")
	}
}

func TestPortableType(t *testing.T) {
	tests := []struct {
		typeName string
		maxLen   int64
		want     dbschema.Type
		length   *int
	}{
		{"bit", 1, dbschema.Bool, nil},
		{"BIGINT", 8, dbschema.Int, nil},
		{"float", 8, dbschema.Float, nil},
		{"varchar", 50, dbschema.String, intPtr(50)},
		{"varchar", -1, dbschema.String, nil},
		{"nchar", 20, dbschema.String, intPtr(10)},
		{"varbinary", 16, dbschema.Bytes, intPtr(16)},
		{"rowversion", 8, dbschema.Bytes, nil},
		{"datetimeoffset", 10, dbschema.Time, nil},
		{"uniqueidentifier", 16, dbschema.String, nil},
	}
	for _, tt := range tests {
		got, length, prec := portableType(tt.typeName, tt.maxLen, 0, 0)
		if got != tt.want || !reflect.DeepEqual(length, tt.length) || prec != nil {
			t.Errorf("%s(%d): got %v %v %v", tt.typeName, tt.maxLen, got, length, prec)
		}
	}
	if _, _, prec := portableType("money", 8, 19, 4); prec == nil || *prec != (dbschema.Precision{Total: 19, Scale: 4}) {
		t.Errorf("money precision %v", prec)
	}
}

func TestStripOuterParens(t *testing.T) {
	for in, want := range map[string]string{
		"((0))":               "0",
		"(getdate())":         "getdate()",
		"((1)+(2))":           "(1)+(2)",
		"('a)b')":             "'a)b'",
		"(abc":                "(abc",
		"x":                   "x",
		"":                    "",
		"(N'it''s')":          "N'it''s'",
		"(newsequentialid())": "newsequentialid()",
	} {
		if got := stripOuterParens(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
