package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// The account setup creates for the backups
const (
	backupUser = "mariadb-backup"
	backupHost = "localhost"
)

// backupPrivileges are what mariadb-backup needs according to the MariaDB documentation
var backupPrivileges = []string{"RELOAD", "PROCESS", "LOCK TABLES", "BINLOG MONITOR"}

// noHome replaces HOME for the commands that log in with the credentials
// file. The client libraries read ~/.my.cnf after --defaults-extra-file, so a
// password of root there would override the one of the backup account. The
// global option files in /etc/mysql are still read: the socket and the
// datadir come from there.
const noHome = "/nonexistent"

// credentialsEnv is the environment of the commands that log in with the credentials file
func credentialsEnv() []string {
	var env []string
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "HOME=") {
			env = append(env, variable)
		}
	}
	return append(env, "HOME="+noHome)
}

// account is a MariaDB account, 'user'@'host'
type account struct {
	user string
	host string
}

func (a account) String() string {
	return a.user + "@" + a.host
}

// sql returns the account quoted for a statement
func (a account) sql() string {
	return quoteSQL(a.user) + "@" + quoteSQL(a.host)
}

var sqlEscaper = strings.NewReplacer(`\`, `\\`, `'`, `\'`)

func quoteSQL(value string) string {
	return "'" + sqlEscaper.Replace(value) + "'"
}

// parseAccount parses the user@host CURRENT_USER() returns. The host never
// has an @, the user may.
func parseAccount(value string) (account, error) {
	at := strings.LastIndex(value, "@")
	if at <= 0 || at == len(value)-1 {
		return account{}, fmt.Errorf("%q is not an account", value)
	}
	return account{user: value[:at], host: value[at+1:]}, nil
}

// query runs SQL statements with the mariadb client and returns the rows.
// With a defaults file it logs in with the credentials from it; without one,
// as the OS user through the socket, which is root with the unix_socket
// authentication on Ubuntu. The statements go through stdin, so a password in
// them never shows in the process list.
func (a *App) query(ctx context.Context, defaultsFile string, statements ...string) ([]string, error) {
	client, err := exec.LookPath(a.cfg.MariadbPath)
	if err != nil {
		return nil, fmt.Errorf("the mariadb client is not found (mariadbPath %q): %w", a.cfg.MariadbPath, err)
	}

	var args []string
	if defaultsFile != "" {
		// The client reads the option files only from the first argument
		args = append(args, "--defaults-extra-file="+defaultsFile)
	}
	args = append(args, "--batch", "--skip-column-names")

	cmd := exec.CommandContext(ctx, client, args...)
	// The login as root through the socket keeps the environment: the
	// ~/.my.cnf of root is its own business there
	if defaultsFile != "" {
		cmd.Env = credentialsEnv()
	}
	cmd.Stdin = strings.NewReader(strings.Join(statements, "\n") + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("mariadb: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var rows []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if line != "" {
			rows = append(rows, line)
		}
	}
	return rows, nil
}

// loginWithCredentials returns the account the credentials file logs in as.
// The mariadb client reads the [client] group only: with the user in
// [mariadb-backup] alone it finds none and silently logs in as the OS user
// through the socket. Such a login is not the one of the file and is refused.
// socketAccount is who the OS user logs in as, empty when it cannot.
func (a *App) loginWithCredentials(ctx context.Context, socketAccount string) (string, error) {
	current, err := a.queryRow(ctx, a.cfg.CredentialsFile, "SELECT CURRENT_USER();")
	if err != nil {
		return "", err
	}
	if current == socketAccount {
		return "", fmt.Errorf("it logs in as %s through the socket, not with a user of its own: the user and the password must be in the [client] group", current)
	}
	return current, nil
}

// queryRow runs a statement that returns one row
func (a *App) queryRow(ctx context.Context, defaultsFile, statement string) (string, error) {
	rows, err := a.query(ctx, defaultsFile, statement)
	if err != nil {
		return "", err
	}
	if len(rows) != 1 {
		return "", fmt.Errorf("%s returned %d rows, expected 1", statement, len(rows))
	}
	return rows[0], nil
}

// grantExpression splits a line of SHOW GRANTS into the privileges and their target
var grantExpression = regexp.MustCompile(`^GRANT (.+?) ON (\S+) TO `)

// parseGrants returns the global privileges of SHOW GRANTS and the grants on
// anything else. Only the privileges and targets are taken: the lines also
// carry the password hash, which must not get into the logs.
func parseGrants(grants []string) (global map[string]bool, other []string) {
	global = make(map[string]bool)
	for _, grant := range grants {
		matches := grantExpression.FindStringSubmatch(grant)
		if matches == nil {
			continue
		}
		if matches[2] != "*.*" {
			other = append(other, matches[1]+" ON "+matches[2])
			continue
		}
		for _, privilege := range strings.Split(matches[1], ",") {
			global[strings.ToUpper(strings.TrimSpace(privilege))] = true
		}
	}
	return global, other
}

// missingPrivileges returns the backup privileges the grants lack
func missingPrivileges(grants []string) []string {
	global, _ := parseGrants(grants)
	if global["ALL PRIVILEGES"] {
		return nil
	}
	var missing []string
	for _, privilege := range backupPrivileges {
		if !global[privilege] {
			missing = append(missing, privilege)
		}
	}
	return missing
}

// extraGrants returns what the grants give beyond the backup privileges
func extraGrants(grants []string) []string {
	global, other := parseGrants(grants)
	needed := map[string]bool{"USAGE": true}
	for _, privilege := range backupPrivileges {
		needed[privilege] = true
	}
	var extra []string
	for privilege := range global {
		if !needed[privilege] {
			extra = append(extra, privilege+" ON *.*")
		}
	}
	sort.Strings(extra)
	return append(extra, other...)
}

// The versions in the outputs of mariadb-backup --version and SELECT VERSION()
var (
	backupVersionExpression = regexp.MustCompile(`server (\d+\.\d+\.\d+)`)
	serverVersionExpression = regexp.MustCompile(`^(\d+\.\d+\.\d+)`)
)

// mariadbBackupVersion returns the first line of mariadb-backup --version
func mariadbBackupVersion(ctx context.Context, path string) (string, error) {
	output, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w: %s", path, err, strings.TrimSpace(string(output)))
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(output)), "\n")
	return line, nil
}

// sameVersions reports whether mariadb-backup is built for the version of the server
func sameVersions(backupVersion, serverVersion string) (bool, error) {
	backup := backupVersionExpression.FindStringSubmatch(backupVersion)
	server := serverVersionExpression.FindStringSubmatch(serverVersion)
	if backup == nil || server == nil {
		return false, fmt.Errorf("cannot compare the versions %q and %q", backupVersion, serverVersion)
	}
	return backup[1] == server[1], nil
}
