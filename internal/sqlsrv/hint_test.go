package sqlsrv

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

func azureCfg() ConnConfig {
	return ConnConfig{
		Server:   "myserver.database.windows.net",
		Database: "appdb",
		User:     "appuser@myserver",
	}
}

func loginFailed() error {
	return mssql.Error{Number: errLoginFailed, Message: "Login failed for user 'appuser'."}
}

func TestConnectHintOnAzureLoginFailure(t *testing.T) {
	got := ConnectHint(azureCfg(), loginFailed())
	if got == "" {
		t.Fatal("18456 against Azure should produce a hint")
	}
	// All three causes have to be named: the point of the hint is that the
	// error itself cannot distinguish them.
	for _, want := range []string{
		"password is wrong",
		`"appdb" does not exist`,
		`has no user in "appdb"`,
		"CREATE USER [appuser] FOR LOGIN [appuser]",
		"--schema-only",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hint is missing %q:\n%s", want, got)
		}
	}
	// The "@servername" suffix belongs to the login, not to the database user.
	if strings.Contains(got, "CREATE USER [appuser@myserver]") {
		t.Errorf("the database user should not carry the server suffix:\n%s", got)
	}
}

func TestConnectHintIsWrappedIntoTheError(t *testing.T) {
	// What Open does, so the shape of the final message is covered too.
	c := azureCfg()
	err := fmt.Errorf("connect to %s: %w\n\n%s", c.Redacted(), loginFailed(), ConnectHint(c, loginFailed()))
	if !strings.Contains(err.Error(), "Login failed for user") {
		t.Error("the driver's own message must survive")
	}
	if !strings.Contains(err.Error(), "Azure SQL authenticates against the target database") {
		t.Error("the hint should follow the error")
	}
}

func TestConnectHintStaysQuietWhenItHasNothingToAdd(t *testing.T) {
	cases := []struct {
		name string
		cfg  ConnConfig
		err  error
	}{
		{"not a SQL Server error", azureCfg(), errors.New("dial tcp: i/o timeout")},
		{"an error with no advice attached", azureCfg(), mssql.Error{Number: 208, Message: "Invalid object name 'x'."}},
		{"a local server, where 18456 means the password", ConnConfig{Server: `DEVBOX\SQLEXPRESS`, Database: "AppDb", User: "sa"}, loginFailed()},
	}
	for _, c := range cases {
		if got := ConnectHint(c.cfg, c.err); got != "" {
			t.Errorf("%s: expected no hint, got:\n%s", c.name, got)
		}
	}
}

func TestConnectHintForTheErrorsAzureDoesDistinguish(t *testing.T) {
	cases := map[int32]string{
		errIPNotAllowed:     "firewall",
		errDatabaseUnavail:  "pause when idle",
		errCannotOpenServer: "user@servername",
	}
	for number, want := range cases {
		got := ConnectHint(azureCfg(), mssql.Error{Number: number})
		if !strings.Contains(got, want) {
			t.Errorf("error %d: hint %q does not mention %q", number, got, want)
		}
	}
	// 4060 is worth explaining even off Azure: the login exists, the database
	// is simply not open to it.
	got := ConnectHint(ConnConfig{Server: "localhost", Database: "AppDb", User: "sa"},
		mssql.Error{Number: errCannotOpenDB})
	if !strings.Contains(got, `no access to "AppDb"`) {
		t.Errorf("4060 on a local server should still explain itself, got %q", got)
	}
}

func TestConnectHintFindsTheLoginInADSN(t *testing.T) {
	cases := []ConnConfig{
		{DSN: "sqlserver://appuser%40myserver:pw@myserver.database.windows.net?database=appdb"},
		{DSN: `Server=myserver.database.windows.net;Database=appdb;User ID=appuser@myserver;Password=pw`},
		{DSN: "sqlserver://myserver.database.windows.net?database=appdb&user id=appuser%40myserver&password=pw"},
	}
	for _, c := range cases {
		if got := c.LoginName(); got != "appuser@myserver" {
			t.Errorf("%q -> LoginName() = %q", c.DSN, got)
		}
		if hint := ConnectHint(c, loginFailed()); !strings.Contains(hint, "CREATE USER [appuser]") {
			t.Errorf("%q -> hint did not name the login:\n%s", c.DSN, hint)
		}
	}
	// Windows authentication has no login to name.
	if got := (ConnConfig{Server: "localhost", Trusted: true}).LoginName(); got != "" {
		t.Errorf("trusted connection should have no login name, got %q", got)
	}
}
