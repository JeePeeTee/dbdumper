// Command dbdumper exports a SQL Server database (schema + data) into a
// portable archive, and restores that archive into another database.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/JeePeeTee/dbdumper/internal/export"
	"github.com/JeePeeTee/dbdumper/internal/sqlsrv"
)

var version = "dev"

// status owns stderr so that transient progress and permanent lines cannot
// tread on each other.
var status = newStatusLine(os.Stderr)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// The console code page belongs to the window, not to this process, so it
	// is put back before returning however the command ends.
	if status.IsTerminal() && enableUnicodeOutput() {
		useUnicodeGlyphs()
	}
	defer restoreConsole()

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "export", "dump":
		err = runExport(ctx, os.Args[2:])
	case "import", "restore":
		err = runImport(ctx, os.Args[2:])
	case "inspect", "info":
		err = runInspect(os.Args[2:])
	case "verify":
		err = runVerify(ctx, os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("dbdumper", version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "\ncancelled")
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `dbdumper - dump and restore SQL Server databases (schema + data)

Usage:
  dbdumper export  [flags] --out <file.dbdump>
  dbdumper import  [flags] --in  <file.dbdump>
  dbdumper verify  [flags] --in  <file.dbdump>
  dbdumper inspect --in <file.dbdump>

Connection flags (both export and import):
  --server        host, host\instance or host,port
  --database      database name
  --user          SQL login (omit for Windows authentication)
  --password      password for --user (or set DBDUMPER_PASSWORD)
  --trusted       force Windows authentication (default when --user is empty)
  --encrypt       disable | false | true | strict (auto "true" for Azure SQL)
  --trust-cert    accept a self-signed server certificate
  --protocol      pin transport: tcp | np (named pipes) | lpc (shared memory)
  --packet-size   TDS packet size 512..32767 (default 32767; -1 = driver default)
  --dsn           full connection string; overrides the above except
                  --database and --packet-size

Run "dbdumper export -h" or "dbdumper import -h" for the rest.
`)
}

// connFlags registers the shared connection flags on fs.
func connFlags(fs *flag.FlagSet) *sqlsrv.ConnConfig {
	c := &sqlsrv.ConnConfig{AppName: "dbdumper"}
	fs.StringVar(&c.DSN, "dsn", "", "full connection string (overrides the other connection flags, except --database and --packet-size)")
	fs.StringVar(&c.Server, "server", "localhost", `server: host, host\instance or host,port`)
	fs.IntVar(&c.Port, "port", 0, "TCP port (default: instance default)")
	fs.StringVar(&c.Database, "database", "", "database name; with --dsn, overrides the database in it")
	fs.StringVar(&c.User, "user", "", "SQL login (omit for Windows authentication)")
	fs.StringVar(&c.Password, "password", "", "password for --user")
	fs.BoolVar(&c.Trusted, "trusted", false, "force Windows authentication")
	fs.StringVar(&c.Encrypt, "encrypt", "disable", "encryption: disable | false | true | strict (forced to true for Azure SQL)")
	fs.BoolVar(&c.TrustCert, "trust-cert", true, "accept a self-signed server certificate (off by default for Azure SQL)")
	fs.StringVar(&c.Protocol, "protocol", "", "pin the transport: tcp, np (named pipes) or lpc (shared memory)")
	fs.IntVar(&c.PacketSize, "packet-size", sqlsrv.DefaultPacketSize, "TDS packet size in bytes, 512..32767; -1 leaves the driver default of 4096")
	return c
}

func finishConn(fs *flag.FlagSet, c *sqlsrv.ConnConfig) error {
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	// A password on the command line ends up in shell history and in the
	// process list, so allow it to arrive out of band instead.
	passwordFromEnv := false
	if c.Password == "" {
		if env := os.Getenv("DBDUMPER_PASSWORD"); env != "" {
			c.Password, passwordFromEnv = env, true
		}
	}

	// No login name means Windows authentication, and a password without one
	// is unusable either way: the connection string builder needs a user to
	// attach it to. Which mistake it is decides how loudly to complain.
	if c.User == "" {
		if c.Password != "" {
			if !passwordFromEnv {
				return errors.New("--password was given without --user; add --user, or drop --password to use Windows authentication")
			}
			warnf("DBDUMPER_PASSWORD is set but --user is not; ignoring it and using Windows authentication")
			c.Password = ""
		}
		c.Trusted = true
	}

	// A --dsn is otherwise used verbatim, but --database stays meaningful on
	// top of one: the usual case is a single connection string reused to dump
	// several databases, or to restore into a differently named target.
	if c.DSN != "" && c.Database != "" {
		*c = c.WithDatabase(c.Database)
	}
	*c = c.EnsurePacketSize()

	// Azure SQL mandates encryption and presents a certificate that chains to
	// a public root, so the local-server defaults are wrong in both directions.
	if c.IsAzureSQL() {
		if !explicit["encrypt"] {
			c.Encrypt = "true"
		}
		if !explicit["trust-cert"] {
			c.TrustCert = false
		}
	}
	return nil
}

// stringList is a repeatable flag that also accepts comma-separated values.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*s = append(*s, p)
		}
	}
	return nil
}

func logf(format string, args ...any) {
	status.Print(format, args...)
}

func warnf(format string, args ...any) {
	status.Print("  ! "+format, args...)
}

// showProgress renders one transient status line. On a non-terminal it falls
// back to an ordinary line, so a redirected log still records progress.
func showProgress(p export.Progress) {
	status.Update("%s", renderProgress(p, status.Width()))
}
