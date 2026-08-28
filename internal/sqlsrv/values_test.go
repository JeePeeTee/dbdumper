package sqlsrv

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/JeePeeTee/dbdumper/internal/model"
)

func decodeJSONRow(t *testing.T, line string) []any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	var row []any
	if err := dec.Decode(&row); err != nil {
		t.Fatalf("parse %s: %v", line, err)
	}
	return row
}

func TestDecodeTypes(t *testing.T) {
	cols := []model.Column{
		{Name: "i", TypeName: "int"},
		{Name: "b", TypeName: "bit"},
		{Name: "f", TypeName: "float"},
		{Name: "d", TypeName: "decimal", Precision: 19, Scale: 4},
		{Name: "s", TypeName: "nvarchar", MaxLength: 100},
		{Name: "g", TypeName: "uniqueidentifier"},
		{Name: "bin", TypeName: "varbinary", MaxLength: -1},
		{Name: "dt", TypeName: "datetime2", Scale: 7},
	}
	rc := NewRowCodec(cols)

	row := decodeJSONRow(t, `[42,true,1.5,"12.3400","hi","3F2504E0-4F89-11D3-9A0C-0305E82C3301","3q2+7w==","2026-08-27T13:45:00.1234567"]`)
	got, err := rc.Decode(row, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		int64(42), true, 1.5, "12.3400", "hi",
		"3F2504E0-4F89-11D3-9A0C-0305E82C3301",
		[]byte{0xde, 0xad, 0xbe, 0xef},
		"2026-08-27T13:45:00.1234567",
	}
	if !reflect.DeepEqual(got, want) {
		for i := range want {
			if !reflect.DeepEqual(got[i], want[i]) {
				t.Errorf("column %s: got %#v, want %#v", cols[i].Name, got[i], want[i])
			}
		}
	}
}

// TestDecodeNullTyping locks in the rule discovered by probing a live server:
// binary columns need a nil []byte (an untyped NULL arrives as nvarchar, which
// has no implicit conversion to varbinary), and every other type needs the
// untyped nil (a nil []byte arrives as varbinary, which date/time, float and
// sql_variant reject).
func TestDecodeNullTyping(t *testing.T) {
	cases := []struct {
		typeName  string
		wantBytes bool
	}{
		{"varbinary", true},
		{"binary", true},
		{"image", true},
		{"geography", true},
		{"geometry", true},
		{"hierarchyid", true},
		{"int", false},
		{"float", false},
		{"nvarchar", false},
		{"datetime2", false},
		{"datetimeoffset", false},
		{"date", false},
		{"time", false},
		{"uniqueidentifier", false},
		{"decimal", false},
		{"sql_variant", false},
		{"xml", false},
	}
	for _, c := range cases {
		rc := NewRowCodec([]model.Column{{Name: "c", TypeName: c.typeName}})
		got, err := rc.Decode([]any{nil}, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.typeName, err)
		}
		_, isBytes := got[0].([]byte)
		if isBytes != c.wantBytes {
			t.Errorf("%s: null decoded as %#v, wantBytes=%v", c.typeName, got[0], c.wantBytes)
		}
		if !c.wantBytes && got[0] != nil {
			t.Errorf("%s: expected untyped nil, got %#v", c.typeName, got[0])
		}
	}
}

func TestDecodeRejectsWrongShape(t *testing.T) {
	rc := NewRowCodec([]model.Column{{Name: "a", TypeName: "int"}, {Name: "b", TypeName: "int"}})
	if _, err := rc.Decode([]any{json.Number("1")}, nil); err == nil {
		t.Error("expected an error for a short row")
	}
	if _, err := rc.Decode(decodeJSONRow(t, `["x", 1]`), nil); err == nil {
		t.Error("expected an error for a string in an int column")
	}
}

func TestDecodeAliasTypeUsesBaseType(t *testing.T) {
	// A varbinary hiding behind a user-defined type must still get the binary
	// treatment.
	rc := NewRowCodec([]model.Column{
		{Name: "c", TypeSchema: "dbo", TypeName: "Blob", BaseTypeName: "varbinary"},
	})
	got, err := rc.Decode(decodeJSONRow(t, `["3q2+7w=="]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[0].([]byte); !ok {
		t.Errorf("got %#v, want []byte", got[0])
	}
}

func TestSelectList(t *testing.T) {
	rc := NewRowCodec([]model.Column{{Name: "Id"}, {Name: "Order Line"}, {Name: "we]ird"}})
	if got, want := rc.SelectList(), "[Id], [Order Line], [we]]ird]"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestConnConfigString(t *testing.T) {
	c := ConnConfig{Server: `DEVBOX\SQLEXPRESS`, Database: "CRM", Protocol: "lpc", Trusted: true, Encrypt: "disable"}
	got := c.String()
	for _, want := range []string{"sqlserver://DEVBOX/SQLEXPRESS", "database=CRM", "protocol=lpc", "trusted_connection=true"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from %q", want, got)
		}
	}
	if c.DatabaseName() != "CRM" {
		t.Errorf("DatabaseName() = %q", c.DatabaseName())
	}
	if got := c.WithDatabase("master").DatabaseName(); got != "master" {
		t.Errorf("WithDatabase(master).DatabaseName() = %q", got)
	}
}

func TestRedactedHidesPassword(t *testing.T) {
	cases := []string{
		"sqlserver://sa:hunter2@localhost?database=x",
		"sqlserver://localhost?database=x&user id=sa&password=hunter2",
		`server=localhost;database=x;user id=sa;password=hunter2`,
		`Server=localhost;Database=x;User ID=sa;Pwd=hunter2`,
	}
	for _, dsn := range cases {
		if got := (ConnConfig{DSN: dsn}).Redacted(); strings.Contains(got, "hunter2") {
			t.Errorf("password leaked from %q: %s", dsn, got)
		}
	}
	// The discrete-flag form goes through the URL builder.
	c := ConnConfig{Server: "localhost", Database: "x", User: "sa", Password: "hunter2"}
	if got := c.Redacted(); strings.Contains(got, "hunter2") {
		t.Errorf("password leaked: %s", got)
	}
}

// TestWithDatabaseOverridesDSN covers --database being applied on top of a
// --dsn that already names a database, or names none at all.
func TestWithDatabaseOverridesDSN(t *testing.T) {
	cases := []string{
		"sqlserver://host/inst?database=old&trusted_connection=true",
		"sqlserver://host/inst?trusted_connection=true",
		`server=host\inst;database=old;trusted_connection=true`,
		`Data Source=host\inst;Initial Catalog=old;Integrated Security=SSPI`,
		`server=host\inst;trusted_connection=true`,
	}
	for _, dsn := range cases {
		got := (ConnConfig{DSN: dsn}).WithDatabase("new")
		if name := got.DatabaseName(); name != "new" {
			t.Errorf("%q -> DatabaseName() = %q, want \"new\"", dsn, name)
		}
		if strings.Contains(strings.ToLower(got.String()), "=old") {
			t.Errorf("%q -> old database survived: %s", dsn, got.String())
		}
	}
}

func TestIsAzureSQL(t *testing.T) {
	cases := map[bool][]ConnConfig{
		true: {
			{Server: "myserver.database.windows.net"},
			{Server: "MYSERVER.Database.Windows.Net"},
			{DSN: "sqlserver://myserver.database.windows.net?database=appdb"},
			{DSN: `Server=tcp:myserver.database.windows.net,1433;Database=appdb`},
			{Server: "x.sql.azuresynapse.net"},
		},
		false: {
			{Server: `DEVBOX\SQLEXPRESS`},
			{Server: "localhost"},
			{DSN: `server=DEVBOX\SQLEXPRESS;database=AppDb`},
			// A database named after the suffix must not trip the check on the
			// server name being absent.
			{Server: "localhost", Database: "windows"},
		},
	}
	for want, configs := range cases {
		for _, c := range configs {
			if got := c.IsAzureSQL(); got != want {
				t.Errorf("%+v: IsAzureSQL() = %v, want %v", c, got, want)
			}
		}
	}
}

func TestTableFilter(t *testing.T) {
	f := NewTableFilter([]string{"dbo.*", "Orders"}, []string{"dbo.Audit*"})
	cases := map[[2]string]bool{
		{"dbo", "Customer"}:   true,
		{"dbo", "AuditTrail"}: false,
		{"sales", "Orders"}:   true,
		{"sales", "Other"}:    false,
		{"DBO", "CUSTOMER"}:   true, // case-insensitive
	}
	for in, want := range cases {
		if got := f(in[0], in[1]); got != want {
			t.Errorf("%s.%s = %v, want %v", in[0], in[1], got, want)
		}
	}
	if NewTableFilter(nil, nil) != nil {
		t.Error("an empty filter should be nil so callers can skip it")
	}
}

func TestEnsurePacketSize(t *testing.T) {
	cases := []struct {
		name string
		in   ConnConfig
		want string // substring the resulting DSN must contain
		deny string
	}{
		{
			name: "ADO DSN without one gets the default",
			in:   ConnConfig{DSN: `server=host;database=x`},
			want: "packet size=32767",
		},
		{
			name: "URL DSN without one gets the default",
			in:   ConnConfig{DSN: "sqlserver://host?database=x"},
			want: "packet+size=32767",
		},
		{
			name: "a packet size already in the DSN wins",
			in:   ConnConfig{DSN: `server=host;packet size=4096`},
			want: "packet size=4096",
			deny: "32767",
		},
		{
			name: "case-insensitive match on the existing key",
			in:   ConnConfig{DSN: `server=host;Packet Size=8192`},
			want: "Packet Size=8192",
			deny: "32767",
		},
		{
			name: "an explicit flag value is used instead of the default",
			in:   ConnConfig{DSN: `server=host`, PacketSize: 512},
			want: "packet size=512",
		},
		{
			name: "-1 means leave the driver alone",
			in:   ConnConfig{DSN: `server=host`, PacketSize: -1},
			deny: "packet size",
		},
	}
	for _, c := range cases {
		got := c.in.EnsurePacketSize().String()
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: %q missing from %q", c.name, c.want, got)
		}
		if c.deny != "" && strings.Contains(got, c.deny) {
			t.Errorf("%s: %q should not appear in %q", c.name, c.deny, got)
		}
	}

	// Without a DSN the discrete builder already handles it; nothing to add.
	if got := (ConnConfig{Server: "host"}).EnsurePacketSize().DSN; got != "" {
		t.Errorf("EnsurePacketSize invented a DSN: %q", got)
	}
}

// TestWithDatabaseKeepsOtherParams guards the ADO rewrite used by both
// WithDatabase and EnsurePacketSize against dropping unrelated settings.
func TestWithDatabaseKeepsOtherParams(t *testing.T) {
	got := (ConnConfig{DSN: `server=host;Initial Catalog=old;trusted_connection=true;protocol=lpc`}).
		WithDatabase("new").String()
	for _, want := range []string{"server=host", "trusted_connection=true", "protocol=lpc", "database=new"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from %q", want, got)
		}
	}
	if strings.Contains(strings.ToLower(got), "old") {
		t.Errorf("old catalog survived: %q", got)
	}
}
