package sqlsrv

import (
	"net/url"
	"testing"
)

// TestServerWithCommaPort - "host,port" is how SQL Server's own tools spell a
// port and what --server advertises. It used to be left in the hostname, so the
// connection went to a host literally named "myhost,1433".
func TestServerWithCommaPort(t *testing.T) {
	cases := []struct {
		name     string
		server   string
		port     int
		wantHost string
		wantPort string
		wantPath string
	}{
		{"plain host", "myhost", 0, "myhost", "", ""},
		{"comma port", "myhost,1433", 0, "myhost", "1433", ""},
		{"comma port with spaces", " myhost , 1433 ", 0, "myhost", "1433", ""},
		{"instance", `DEVBOX\SQLEXPRESS`, 0, "DEVBOX", "", "/SQLEXPRESS"},
		{"instance and comma port", `DEVBOX\SQLEXPRESS,1433`, 0, "DEVBOX", "1433", "/SQLEXPRESS"},
		{"--port alone", "myhost", 1500, "myhost", "1500", ""},
		// An explicit flag beats a value buried in another flag's string.
		{"--port wins over comma", "myhost,1433", 1500, "myhost", "1500", ""},
		// Not a port: leave it alone rather than silently truncating a hostname.
		{"comma but not a number", "myhost,notaport", 0, "myhost,notaport", "", ""},
		{"comma with zero", "myhost,0", 0, "myhost,0", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := ConnConfig{Server: c.server, Port: c.port, Database: "AppDb", Trusted: true}.String()
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("%q produced an unparseable connection string %q: %v", c.server, raw, err)
			}
			if u.Hostname() != c.wantHost {
				t.Errorf("host = %q, want %q  (from %q)", u.Hostname(), c.wantHost, raw)
			}
			if u.Port() != c.wantPort {
				t.Errorf("port = %q, want %q  (from %q)", u.Port(), c.wantPort, raw)
			}
			if u.Path != c.wantPath {
				t.Errorf("instance = %q, want %q  (from %q)", u.Path, c.wantPath, raw)
			}
		})
	}
}

// TestCommaPortIsNotAppliedToADSN - a --dsn is passed through verbatim, so
// nothing in it should be reinterpreted.
func TestCommaPortIsNotAppliedToADSN(t *testing.T) {
	dsn := "sqlserver://myhost,1433?database=AppDb"
	if got := (ConnConfig{DSN: dsn}).String(); got != dsn {
		t.Errorf("a DSN should pass through untouched:\n got %q\nwant %q", got, dsn)
	}
}
