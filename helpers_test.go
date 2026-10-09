package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeMariadbBackup stands for mariadb-backup in the tests. It writes the
// checkpoints the way the real one does with --extra-lsndir, continuing the
// LSN of --incremental-basedir, and a fake stream to stdout. The FAKE_*
// variables make it fail in the ways the backuper has to notice.
const fakeMariadbBackup = `#!/bin/sh
if [ "$1" = "--version" ]; then
	echo "mariadb-backup based on MariaDB server 11.4.13-MariaDB Linux (x86_64)"
	exit 0
fi
echo "$*" >> "$FAKE_CALLS"
if [ -n "$FAKE_SLEEP" ]; then
	exec sleep "$FAKE_SLEEP"
fi

lsndir=""
basedir=""
for arg in "$@"; do
	case "$arg" in
		--extra-lsndir=*) lsndir="${arg#--extra-lsndir=}" ;;
		--incremental-basedir=*) basedir="${arg#--incremental-basedir=}" ;;
	esac
done

echo "[00] Connecting to MariaDB server host: localhost" >&2
# The real one reads ~/.my.cnf after --defaults-extra-file
if [ -f "$HOME/.my.cnf" ]; then
	echo "[00] Failed to connect to MariaDB server: Access denied for user 'mariadb-backup'@'localhost' (using password: YES)." >&2
	exit 1
fi
if [ -n "$FAKE_BACKUP_FAIL" ]; then
	echo "[00] FATAL ERROR: fake failure" >&2
	exit 1
fi

if [ -n "$basedir" ]; then
	type=incremental
	from=$(sed -n 's/^to_lsn = //p' "$basedir/mariadb_backup_checkpoints")
else
	type=full-backuped
	from=0
fi
to=$((from + 100))
if [ -n "$FAKE_FROM_LSN" ]; then
	from=$FAKE_FROM_LSN
fi

mkdir -p "$lsndir"
case "$FAKE_CHECKPOINTS" in
	missing) ;;
	garbage) echo "not checkpoints" > "$lsndir/mariadb_backup_checkpoints" ;;
	*) printf 'backup_type = %s\nfrom_lsn = %s\nto_lsn = %s\nlast_lsn = %s\nrecover_binlog_info = 0\n' "$type" "$from" "$to" "$to" > "$lsndir/mariadb_backup_checkpoints" ;;
esac

echo "stream of $type from $from to $to"
echo "[00] completed OK!" >&2
`

// fakeZstd passes the stream through and accepts every archive in -t mode
const fakeZstd = `#!/bin/sh
for arg in "$@"; do
	if [ "$arg" = "-t" ]; then
		if [ -n "$FAKE_ZSTD_TEST_FAIL" ]; then
			echo "zstd: corrupted block detected" >&2
			exit 1
		fi
		exit 0
	fi
done
if [ -n "$FAKE_ZSTD_FAIL" ]; then
	echo "zstd: fake failure" >&2
	exit 1
fi
exec cat
`

// fakeMariadb stands for the mariadb client. The accounts live in $FAKE_DB:
// accounts/<user>@<host> holds the password, grants/<user>@<host> the
// privileges, and the arguments of every run go to $FAKE_DB/args. Like the
// real client it reads only the [client] group of the option file and logs in
// as the OS user without one. It knows only the statements the backuper sends.
const fakeMariadb = `#!/bin/sh
mkdir -p "$FAKE_DB/accounts" "$FAKE_DB/grants"
echo "$*" >> "$FAKE_DB/args"

case "$1" in
	--defaults-extra-file=*)
		file="${1#--defaults-extra-file=}"
		user=$(sed -n '/^\[client\]/,/^\[/s/^user=//p' "$file")
		password=$(sed -n '/^\[client\]/,/^\[/s/^password=//p' "$file")
		# ~/.my.cnf is read after the file and overrides it
		if [ -f "$HOME/.my.cnf" ]; then
			override=$(sed -n '/^\[client\]/,/^\[/s/^password=//p' "$HOME/.my.cnf")
			password=${override:-$password}
		fi
		login="$user@localhost"
		if [ -z "$user" ]; then
			login="root@localhost"
		elif [ ! -f "$FAKE_DB/accounts/$login" ] || [ "$(cat "$FAKE_DB/accounts/$login")" != "$password" ]; then
			echo "ERROR 1045 (28000): Access denied for user '$user'@'localhost' (using password: YES)" >&2
			exit 1
		fi
		;;
	*)
		if [ -n "$FAKE_SERVER_DOWN" ]; then
			echo "ERROR 2002 (HY000): Can't connect to local server through socket '/run/mysqld/mysqld.sock' (2)" >&2
			exit 1
		fi
		login="root@localhost"
		;;
esac

while IFS= read -r statement; do
	account=$(printf '%s\n' "$statement" | sed -n "s/.* '\([^']*\)'@'\([^']*\)'.*/\1@\2/p")
	password=$(printf '%s\n' "$statement" | sed -n "s/.* IDENTIFIED BY '\([^']*\)';$/\1/p")
	case "$statement" in
		"SELECT VERSION();")
			echo "${FAKE_SERVER_VERSION:-11.4.13-MariaDB-ubu2404}" ;;
		"SELECT CURRENT_USER();")
			echo "$login" ;;
		"SELECT COUNT(*) FROM mysql.user WHERE "*)
			account=$(printf '%s\n' "$statement" | sed -n "s/.*User='\([^']*\)' AND Host='\([^']*\)'.*/\1@\2/p")
			if [ -f "$FAKE_DB/accounts/$account" ]; then echo 1; else echo 0; fi ;;
		"CREATE USER IF NOT EXISTS "*)
			if [ -n "$FAKE_CREATE_FAIL" ]; then
				echo "ERROR 1396 (HY000) at line 1: Operation CREATE USER failed near '$statement'" >&2
				exit 1
			fi
			[ -f "$FAKE_DB/accounts/$account" ] || printf '%s' "$password" > "$FAKE_DB/accounts/$account" ;;
		"ALTER USER "*)
			printf '%s' "$password" > "$FAKE_DB/accounts/$account" ;;
		"GRANT "*)
			privileges=$(printf '%s\n' "$statement" | sed -n 's/^GRANT \(.*\) ON \*\.\* TO .*/\1/p')
			if [ -s "$FAKE_DB/grants/$account" ]; then
				privileges="$(cat "$FAKE_DB/grants/$account"), $privileges"
			fi
			printf '%s' "$privileges" > "$FAKE_DB/grants/$account" ;;
		"SHOW GRANTS;"|"SHOW GRANTS FOR "*)
			if [ "$statement" = "SHOW GRANTS;" ]; then
				account=$login
			fi
			privileges=$(cat "$FAKE_DB/grants/$account" 2>/dev/null)
			echo "GRANT ${privileges:-USAGE} ON *.* TO \` + "`" + `${account%@*}\` + "`" + `@\` + "`" + `${account##*@}\` + "`" + ` IDENTIFIED BY PASSWORD '*2470C0C06DEE42FD1618BB99005ADCA2EC9D1E19'" ;;
		*)
			echo "ERROR 1064 (42000) at line 1: unexpected statement $statement" >&2
			exit 1 ;;
	esac
done
`

var fakeVariables = []string{
	"FAKE_SLEEP", "FAKE_BACKUP_FAIL", "FAKE_FROM_LSN", "FAKE_CHECKPOINTS", "FAKE_ZSTD_FAIL", "FAKE_ZSTD_TEST_FAIL",
	"FAKE_SERVER_DOWN", "FAKE_CREATE_FAIL", "FAKE_SERVER_VERSION",
}

// The account and the password the test environment starts with
const (
	testAccount  = "mariadb-backup@localhost"
	testPassword = "secret"
)

// testLocation is the local time of the server in the tests, ahead of UTC
// like Moscow, so the dates of the names and of the rotation differ
var testLocation = time.FixedZone("MSK", 3*60*60)

type testEnv struct {
	t     *testing.T
	app   *App
	calls string
	// db is the directory of the fake MariaDB accounts
	db   string
	now  time.Time
	free uint64
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	root, bin := filepath.Join(dir, "backups"), filepath.Join(dir, "bin")
	for _, path := range []string{root, bin} {
		if err := os.Mkdir(path, 0750); err != nil {
			t.Fatal(err)
		}
	}
	for name, script := range map[string]string{"mariadb-backup": fakeMariadbBackup, "zstd": fakeZstd, "mariadb": fakeMariadb} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	credentials := filepath.Join(dir, "mariadb-backup.cnf")
	if err := os.WriteFile(credentials, []byte("[client]\nuser=mariadb-backup\npassword="+testPassword+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// The server already has the backup account with its privileges
	db := filepath.Join(dir, "db")
	for path, content := range map[string]string{
		filepath.Join(db, "accounts", testAccount): testPassword,
		filepath.Join(db, "grants", testAccount):   strings.Join(backupPrivileges, ", "),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0640); err != nil {
			t.Fatal(err)
		}
	}

	cfg := defaultConfig()
	cfg.BackupRoot = root
	cfg.CredentialsFile = credentials
	cfg.MariadbBackupPath = filepath.Join(bin, "mariadb-backup")
	cfg.ZstdPath = filepath.Join(bin, "zstd")
	cfg.MariadbPath = filepath.Join(bin, "mariadb")

	app, err := newApp(cfg, slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}

	env := &testEnv{t: t, app: app, calls: filepath.Join(dir, "calls"), db: db, free: 1 << 40}
	env.at(2026, 10, 12, 2, 0)
	app.now = func() time.Time { return env.now }
	app.location = testLocation
	app.freeSpace = func(string) (uint64, error) { return env.free, nil }

	t.Setenv("FAKE_CALLS", env.calls)
	t.Setenv("FAKE_DB", env.db)
	for _, name := range fakeVariables {
		t.Setenv(name, "")
	}
	return env
}

// at sets the current time of the server, in its local time
func (e *testEnv) at(year int, month time.Month, day, hour, minute int) {
	e.now = time.Date(year, month, day, hour, minute, 0, 0, testLocation)
}

// nextDay moves the time to 02:00 of the next day, the time of the timer
func (e *testEnv) nextDay() {
	next := e.now.AddDate(0, 0, 1)
	e.at(next.Year(), next.Month(), next.Day(), 2, 0)
}

func (e *testEnv) backup() error {
	return e.app.Backup(context.Background(), false)
}

func (e *testEnv) mustBackup() {
	e.t.Helper()
	if err := e.backup(); err != nil {
		e.t.Fatalf("backup at %v: %v", e.now, err)
	}
}

// published returns the names of the files in publish/weekly
func (e *testEnv) published() []string {
	e.t.Helper()
	return e.list(e.app.layout.publish())
}

func (e *testEnv) list(dir string) []string {
	e.t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func (e *testEnv) state() *State {
	e.t.Helper()
	state, err := loadState(e.app.layout.state(), e.app.cfg.Name)
	if err != nil {
		e.t.Fatal(err)
	}
	return state
}

func (e *testEnv) stateBytes() []byte {
	e.t.Helper()
	data, err := os.ReadFile(e.app.layout.state())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		e.t.Fatal(err)
	}
	return data
}

func (e *testEnv) writeState(content string) {
	e.t.Helper()
	if err := os.WriteFile(e.app.layout.state(), []byte(content), 0640); err != nil {
		e.t.Fatal(err)
	}
}

// callArgs returns the arguments of every backup run of the fake mariadb-backup
func (e *testEnv) callArgs() [][]string {
	e.t.Helper()
	data, err := os.ReadFile(e.calls)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	var calls [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		calls = append(calls, strings.Fields(line))
	}
	return calls
}

func (e *testEnv) lastCallArg(prefix string) string {
	e.t.Helper()
	calls := e.callArgs()
	if len(calls) == 0 {
		e.t.Fatal("mariadb-backup was not run")
	}
	for _, arg := range calls[len(calls)-1] {
		if value, ok := strings.CutPrefix(arg, prefix); ok {
			return value
		}
	}
	return ""
}

// expectNothingStaged checks that no unverified data is left behind
func (e *testEnv) expectNothingStaged() {
	e.t.Helper()
	if staged := e.list(e.app.layout.staging()); len(staged) > 0 {
		e.t.Errorf("staging is not empty: %v", staged)
	}
}

func expectNames(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\ngot  %q\nwant %q", what, got, want)
	}
}
