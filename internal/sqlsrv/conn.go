// Package sqlsrv holds the SQL Server specific plumbing: connecting,
// introspecting a database into a model.Manifest, and converting values
// between TDS and the archive's JSON encoding.
package sqlsrv

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	_ "github.com/microsoft/go-mssqldb"              // tcp
	_ "github.com/microsoft/go-mssqldb/namedpipe"    // np
	_ "github.com/microsoft/go-mssqldb/sharedmemory" // lpc; the only protocol some local instances enable
)

// ConnConfig describes how to reach a SQL Server instance. Either DSN is set
// (and used verbatim), or the discrete fields are assembled into one.
type ConnConfig struct {
	DSN       string
	Server    string
	Port      int
	Database  string
	User      string
	Password  string
	Trusted   bool
	Encrypt   string // disable | false | true | strict
	TrustCert bool
	AppName   string
	// Protocol pins the transport: tcp, np (named pipes) or lpc (shared
	// memory). Empty lets the driver negotiate.
	Protocol string
	// MaxConns caps the pool. It must be at least as large as the importer's
	// parallelism, or workers starve waiting for a connection.
	MaxConns int
	// PacketSize is the TDS packet size in bytes, 512..32767. The driver's
	// default is 4096, which costs a lot of round trips on a high-latency
	// link; zero here means DefaultPacketSize instead.
	PacketSize int
}

// DefaultPacketSize is the TDS packet size the tool asks for. It is the
// protocol maximum; an encrypted connection is negotiated down to 16383 by the
// server, which is still four times the driver's 4096-byte default.
const DefaultPacketSize = 32767

// String builds the go-mssqldb URL for this config.
func (c ConnConfig) String() string {
	if c.DSN != "" {
		return c.DSN
	}
	u := &url.URL{Scheme: "sqlserver"}

	host := c.Server
	if host == "" {
		host = "localhost"
	}
	// "host,port" is how SQL Server's own tools spell a port, and --server
	// advertises it. Left in place it becomes part of the hostname and the
	// connection goes nowhere useful.
	port := c.Port
	if i := strings.LastIndex(host, ","); i >= 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(host[i+1:])); err == nil && n > 0 {
			host = strings.TrimSpace(host[:i])
			if port == 0 {
				// An explicit --port wins; this is only the fallback.
				port = n
			}
		}
	}
	// "host\instance" must go into the path, not the authority.
	if i := strings.Index(host, `\`); i >= 0 {
		u.Host = host[:i]
		u.Path = host[i+1:]
	} else {
		u.Host = host
	}
	if port > 0 {
		u.Host = fmt.Sprintf("%s:%d", u.Host, port)
	}
	if !c.Trusted && c.User != "" {
		u.User = url.UserPassword(c.User, c.Password)
	}

	q := url.Values{}
	if c.Database != "" {
		q.Set("database", c.Database)
	}
	if c.AppName != "" {
		q.Set("app name", c.AppName)
	}
	if c.Encrypt != "" {
		q.Set("encrypt", c.Encrypt)
	}
	if c.TrustCert {
		q.Set("trustservercertificate", "true")
	}
	if c.Trusted {
		q.Set("trusted_connection", "true")
	}
	if c.Protocol != "" {
		q.Set("protocol", c.Protocol)
	}
	size := c.PacketSize
	if size == 0 {
		size = DefaultPacketSize
	}
	if size > 0 {
		q.Set("packet size", strconv.Itoa(size))
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// IsAzureSQL reports whether this config points at Azure SQL Database or a
// Synapse/Fabric endpoint, which differ from a boxed SQL Server in ways the
// tool has to account for: encryption is mandatory, and a database cannot be
// put into SINGLE_USER mode.
func (c ConnConfig) IsAzureSQL() bool {
	haystack := c.Server
	if c.DSN != "" {
		haystack = c.DSN
	}
	haystack = strings.ToLower(haystack)
	for _, suffix := range []string{
		".database.windows.net",
		".database.chinacloudapi.cn",
		".database.usgovcloudapi.net",
		".sql.azuresynapse.net",
		".datawarehouse.fabric.microsoft.com",
	} {
		if strings.Contains(haystack, suffix) {
			return true
		}
	}
	return false
}

// Redacted returns the connection string with the password removed, for logs.
// It handles both the URL form and the ADO "key=value;key=value" form, since a
// user-supplied --dsn may be either.
func (c ConnConfig) Redacted() string {
	s := c.String()
	if u, err := url.Parse(s); err == nil && u.Scheme == "sqlserver" {
		if u.User != nil {
			if _, ok := u.User.Password(); ok {
				u.User = url.UserPassword(u.User.Username(), "***")
			}
		}
		q := u.Query()
		if q.Get("password") != "" {
			q.Set("password", "***")
			u.RawQuery = q.Encode()
		}
		return u.String()
	}

	parts := strings.Split(s, ";")
	for i, p := range parts {
		k, _, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "password", "pwd":
			parts[i] = strings.TrimSpace(k) + "=***"
		}
	}
	return strings.Join(parts, ";")
}

// Open dials the server and verifies the connection.
func Open(ctx context.Context, c ConnConfig) (*sql.DB, error) {
	db, err := sql.Open("sqlserver", c.String())
	if err != nil {
		return nil, fmt.Errorf("open sqlserver: %w", err)
	}
	maxConns := c.MaxConns
	if maxConns <= 0 {
		maxConns = 8
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		if hint := ConnectHint(c, err); hint != "" {
			return nil, fmt.Errorf("connect to %s: %w\n\n%s", c.Redacted(), err, hint)
		}
		return nil, fmt.Errorf("connect to %s: %w", c.Redacted(), err)
	}
	return db, nil
}

// WithDatabase returns a copy of the config pointed at a different database.
func (c ConnConfig) WithDatabase(name string) ConnConfig {
	if c.DSN == "" {
		c.Database = name
		return c
	}
	if u, err := url.Parse(c.DSN); err == nil && u.Scheme == "sqlserver" {
		q := u.Query()
		q.Set("database", name)
		u.RawQuery = q.Encode()
		c.DSN = u.String()
		return c
	}
	c.DSN = setADOParam(c.DSN, "database", name, "initial catalog")
	return c
}

// EnsurePacketSize adds a TDS packet size to a DSN that does not already name
// one. A --dsn is otherwise taken verbatim, but this flag has a default the
// tool chose rather than the user, and it is exactly the DSN users on a
// high-latency link who need it: silently leaving them on the driver's 4096
// bytes contradicts what --help says the default is. A packet size already in
// the DSN always wins.
func (c ConnConfig) EnsurePacketSize() ConnConfig {
	size := c.PacketSize
	if size == 0 {
		size = DefaultPacketSize
	}
	if c.DSN == "" || size < 0 || dsnHasParam(c.DSN, "packet size") {
		return c
	}
	if u, err := url.Parse(c.DSN); err == nil && u.Scheme == "sqlserver" {
		q := u.Query()
		q.Set("packet size", strconv.Itoa(size))
		u.RawQuery = q.Encode()
		c.DSN = u.String()
		return c
	}
	c.DSN = setADOParam(c.DSN, "packet size", strconv.Itoa(size))
	return c
}

// dsnHasParam reports whether an ADO-style or URL DSN already sets key.
func dsnHasParam(dsn, key string) bool {
	if u, err := url.Parse(dsn); err == nil && u.Scheme == "sqlserver" {
		_, ok := u.Query()[key]
		return ok
	}
	for _, p := range strings.Split(dsn, ";") {
		k, _, ok := strings.Cut(p, "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), key) {
			return true
		}
	}
	return false
}

// setADOParam replaces key in an ADO-style "key=value;..." string, dropping any
// of its aliases on the way.
func setADOParam(dsn, key, value string, aliases ...string) string {
	drop := append([]string{key}, aliases...)
	parts := strings.Split(dsn, ";")
	out := parts[:0]
	for _, p := range parts {
		k := strings.ToLower(strings.TrimSpace(strings.SplitN(p, "=", 2)[0]))
		skip := false
		for _, d := range drop {
			if k == d {
				skip = true
				break
			}
		}
		if skip || strings.TrimSpace(p) == "" {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(append(out, key+"="+value), ";")
}

// LoginName reports the SQL login this config authenticates as, if it can tell.
// Windows authentication has none, and returns "".
func (c ConnConfig) LoginName() string {
	if c.DSN == "" {
		return c.User
	}
	if u, err := url.Parse(c.DSN); err == nil && u.Scheme == "sqlserver" {
		if u.User != nil && u.User.Username() != "" {
			return u.User.Username()
		}
		return u.Query().Get("user id")
	}
	for _, p := range strings.Split(c.DSN, ";") {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(kv[0])) {
		case "user id", "user", "uid":
			return strings.TrimSpace(kv[1])
		}
	}
	return ""
}

// DatabaseName reports the database this config points at, if it can tell.
func (c ConnConfig) DatabaseName() string {
	if c.DSN == "" {
		return c.Database
	}
	if u, err := url.Parse(c.DSN); err == nil && u.Scheme == "sqlserver" {
		return u.Query().Get("database")
	}
	for _, p := range strings.Split(c.DSN, ";") {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(kv[0])) {
		case "database", "initial catalog":
			return strings.TrimSpace(kv[1])
		}
	}
	return ""
}
