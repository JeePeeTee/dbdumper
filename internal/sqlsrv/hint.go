package sqlsrv

import (
	"errors"
	"fmt"
	"strings"

	mssql "github.com/microsoft/go-mssqldb"
)

// SQL Server error numbers that Azure SQL returns for connection problems it
// deliberately does not distinguish, plus the two it does.
const (
	errLoginFailed      = 18456 // wrong password, unknown database, or no user in it
	errCannotOpenDB     = 4060  // the database exists but the login cannot enter it
	errCannotOpenServer = 40532 // login name not in the form the gateway could route
	errIPNotAllowed     = 40615 // client address is not in the server firewall rules
	errDatabaseUnavail  = 40613 // database is paused, scaling, or otherwise not available
	errLoginDisabled    = 18470 // the login exists and the password matched, but it is disabled
)

// ConnectHint turns a connection failure into advice, where the error alone is
// not enough to act on.
//
// Azure SQL authenticates against the target database rather than the server,
// and reports a wrong password, a database that does not exist, and a login
// with no user in that database all as 18456 "Login failed for user". That is
// deliberate - distinguishing them would tell an attacker which databases exist
// - but it leaves a legitimate user with no idea which of the three to fix.
//
// It returns "" when there is nothing useful to add.
func ConnectHint(c ConnConfig, err error) string {
	var e mssql.Error
	if !errors.As(err, &e) {
		return ""
	}

	switch e.Number {
	case errIPNotAllowed:
		return "The server's firewall is refusing this client's IP address. Add it under\n" +
			"  Networking > Firewall rules for the server in the Azure portal."
	case errDatabaseUnavail:
		return "The database is not currently available - serverless databases pause when idle and\n" +
			"take a moment to resume, and a scaling operation has the same effect. Retry shortly."
	case errCannotOpenServer:
		return "Azure could not route the login to a server. Check the server name, and that the\n" +
			"login is given as \"user@servername\" if your server requires that form."
	case errLoginDisabled:
		// Unlike 18456, this one is not ambiguous: the server has already
		// accepted the name and the password and is refusing on policy. Sending
		// the reader off to check for typos and missing users would be wrong.
		login := c.LoginName()
		if login == "" {
			return "The login is disabled. An administrator can re-enable it with ALTER LOGIN ... ENABLE."
		}
		return fmt.Sprintf("The login %q exists and the password was accepted, but the account is disabled.\n"+
			"Connected as an administrator:\n\n"+
			"    ALTER LOGIN [%s] ENABLE;", login, login)
	case errLoginFailed, errCannotOpenDB:
		// Handled below, where the advice depends on the target.
	default:
		return ""
	}

	if !c.IsAzureSQL() {
		if e.Number == errCannotOpenDB {
			return fmt.Sprintf("The login exists but has no access to %q. Grant it a user in that database.",
				c.DatabaseName())
		}
		return ""
	}

	db := c.DatabaseName()
	if db == "" {
		db = "<database>"
	}
	login := c.LoginName()
	user := login
	if i := strings.Index(user, "@"); i > 0 {
		// The "@servername" suffix belongs to the login as the gateway sees it;
		// the database user is the part in front of it.
		user = user[:i]
	}
	if user == "" {
		user = "<login>"
	}

	return fmt.Sprintf(`Azure SQL authenticates against the target database, and reports all three of these
as the same error. Any one of them could be the cause:

  1. The password is wrong.
     It is read from --password, or from DBDUMPER_PASSWORD when that is absent.

  2. The database %q does not exist on this server.
     Names on Azure are case-insensitive but must otherwise match exactly.

  3. The login %q has no user in %q.
     A login is server-level; access is granted per database. A database that was
     newly created, restored, or copied under another name will not have the user
     until it is added. Connected to %q as an administrator:

         CREATE USER [%s] FOR LOGIN [%s];
         ALTER ROLE db_datareader ADD MEMBER [%s];
         GRANT VIEW DEFINITION TO [%s];
         GRANT VIEW DATABASE STATE TO [%s];

To tell them apart, try the same credentials against a database you know the login
can reach, with --schema-only. If that works, the password is not the problem.`,
		db, login, db, db, user, user, user, user, user)
}
