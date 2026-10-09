package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// accountPassword returns the password the fake server has for the account
func (e *testEnv) accountPassword(name string) string {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.db, "accounts", name))
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return string(data)
}

func (e *testEnv) removeAccount() {
	e.t.Helper()
	for _, dir := range []string{"accounts", "grants"} {
		if err := os.Remove(filepath.Join(e.db, dir, testAccount)); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *testEnv) removeCredentials() {
	e.t.Helper()
	if err := os.Remove(e.app.cfg.CredentialsFile); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) setup() (string, error) {
	var out bytes.Buffer
	err := e.app.Setup(context.Background(), &out)
	return out.String(), err
}

var credentialsPassword = regexp.MustCompile(`(?m)^password=(.*)$`)

// expectWorkingCredentials checks that the credentials file is private and
// logs in to the account, and returns its password
func (e *testEnv) expectWorkingCredentials() string {
	e.t.Helper()
	path := e.app.cfg.CredentialsFile
	info, err := os.Stat(path)
	if err != nil {
		e.t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		e.t.Errorf("the credentials file has mode %v, want 0600", info.Mode().Perm())
	}

	content, err := os.ReadFile(path)
	if err != nil {
		e.t.Fatal(err)
	}
	matches := credentialsPassword.FindSubmatch(content)
	if matches == nil || !strings.Contains(string(content), "[client]\nuser=mariadb-backup\n") {
		e.t.Fatalf("the credentials file:\n%s", content)
	}
	password := string(matches[1])
	if got := e.accountPassword(testAccount); got != password {
		e.t.Errorf("the account has the password %q, the file %q", got, password)
	}
	return password
}

func TestSetupCreatesTheAccount(t *testing.T) {
	env := newTestEnv(t)
	env.removeAccount()
	env.removeCredentials()

	out, err := env.setup()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	password := env.expectWorkingCredentials()
	if len(password) < 26 || password == testPassword {
		t.Errorf("a weak password %q", password)
	}
	content, err := os.ReadFile(env.app.cfg.CredentialsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(content), credentialsHeader+"\n") {
		t.Errorf("no header in:\n%s", content)
	}

	grants, err := os.ReadFile(filepath.Join(env.db, "grants", testAccount))
	if err != nil {
		t.Fatal(err)
	}
	if string(grants) != strings.Join(backupPrivileges, ", ") {
		t.Errorf("granted %q", grants)
	}

	// The password goes through stdin only
	args, err := os.ReadFile(filepath.Join(env.db, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), password) {
		t.Errorf("the password is among the arguments:\n%s", args)
	}

	if !strings.Contains(out, "OK    the credentials file logs in to MariaDB as "+testAccount) || strings.Contains(out, "FAIL") || strings.Contains(out, "WARN") {
		t.Errorf("the check after the setup:\n%s", out)
	}
}

func TestSetupKeepsWorkingCredentials(t *testing.T) {
	env := newTestEnv(t)
	before, err := os.ReadFile(env.app.cfg.CredentialsFile)
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if out, err := env.setup(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}

	after, err := os.ReadFile(env.app.cfg.CredentialsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || env.accountPassword(testAccount) != testPassword {
		t.Errorf("the working credentials were replaced:\n%s", after)
	}
}

func TestSetupReplacesCredentialsThatDoNotWork(t *testing.T) {
	cases := map[string]func(env *testEnv){
		"the account exists without the file": func(env *testEnv) {
			env.removeCredentials()
		},
		"the file has a wrong password": func(env *testEnv) {
			if err := os.WriteFile(env.app.cfg.CredentialsFile, []byte("[client]\nuser=mariadb-backup\npassword=old\n"), 0600); err != nil {
				env.t.Fatal(err)
			}
		},
		// The mariadb client ignores the group and logs in as root through the socket
		"the file has the user in [mariadb-backup] only": func(env *testEnv) {
			if err := os.WriteFile(env.app.cfg.CredentialsFile, []byte("[mariadb-backup]\nuser=mariadb-backup\npassword="+testPassword+"\n"), 0600); err != nil {
				env.t.Fatal(err)
			}
		},
		"the file has no account": func(env *testEnv) {
			env.removeAccount()
		},
	}
	for name, breakCredentials := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			breakCredentials(env)

			if out, err := env.setup(); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if password := env.expectWorkingCredentials(); password == testPassword {
				t.Error("the password is not new")
			}
		})
	}
}

func TestSetupMakesTheCredentialsFilePrivate(t *testing.T) {
	env := newTestEnv(t)
	if err := os.Chmod(env.app.cfg.CredentialsFile, 0644); err != nil {
		t.Fatal(err)
	}
	if out, err := env.setup(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if password := env.expectWorkingCredentials(); password != testPassword {
		t.Error("the working password was replaced")
	}
}

// The ~/.my.cnf of root is read by the client libraries after the
// credentials file: its password must not override the one of the account
func TestPersonalOptionFileOfRootIsIgnored(t *testing.T) {
	env := newTestEnv(t)
	env.removeAccount()
	env.removeCredentials()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".my.cnf"), []byte("[client]\npassword=rootpw\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	if out, err := env.setup(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	env.expectWorkingCredentials()
	env.mustBackup()
}

func TestSetupWithoutTheServer(t *testing.T) {
	env := newTestEnv(t)
	env.removeCredentials()
	t.Setenv("FAKE_SERVER_DOWN", "1")

	_, err := env.setup()
	if err == nil || !strings.Contains(err.Error(), "through the socket") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(env.app.cfg.CredentialsFile); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the credentials file: %v", err)
	}
}

// The client quotes a failed statement in its error, the password must not
// get into the log with it
func TestSetupHidesThePasswordOfAFailedStatement(t *testing.T) {
	env := newTestEnv(t)
	env.removeAccount()
	env.removeCredentials()
	t.Setenv("FAKE_CREATE_FAIL", "1")

	_, err := env.setup()
	if err == nil {
		t.Fatal("the setup succeeded")
	}
	if !strings.Contains(err.Error(), "IDENTIFIED BY '<password>'") {
		t.Errorf("the password is not hidden: %v", err)
	}
	if _, err := os.Stat(env.app.cfg.CredentialsFile); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the credentials file of an account that was not created: %v", err)
	}
}

func TestSetupPointsOutExtraPrivileges(t *testing.T) {
	env := newTestEnv(t)
	if err := os.WriteFile(filepath.Join(env.db, "grants", testAccount), []byte("SUPER, RELOAD"), 0640); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	env.app.log = slog.New(slog.NewTextHandler(&log, nil))

	if out, err := env.setup(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(log.String(), `grant="SUPER ON *.*"`) {
		t.Errorf("no warning about SUPER:\n%s", log.String())
	}
	// SHOW GRANTS carries the hash of the password
	if strings.Contains(log.String(), "2470C0C06DEE42FD") {
		t.Errorf("the password hash is logged:\n%s", log.String())
	}
}

func TestCheckLogsInToTheDatabase(t *testing.T) {
	cases := map[string]struct {
		breakIt func(env *testEnv)
		fail    string
		warn    string
	}{
		"a wrong password": {
			breakIt: func(env *testEnv) { env.removeAccount() },
			fail:    "the credentials file logs in to MariaDB",
		},
		"the user in [mariadb-backup] only": {
			breakIt: func(env *testEnv) {
				if err := os.WriteFile(env.app.cfg.CredentialsFile, []byte("[mariadb-backup]\nuser=mariadb-backup\npassword="+testPassword+"\n"), 0600); err != nil {
					env.t.Fatal(err)
				}
			},
			fail: "through the socket, not with a user of its own",
		},
		"missing privileges": {
			breakIt: func(env *testEnv) {
				if err := os.WriteFile(filepath.Join(env.db, "grants", testAccount), []byte("RELOAD"), 0640); err != nil {
					env.t.Fatal(err)
				}
			},
			fail: "PROCESS, LOCK TABLES, BINLOG MONITOR missing",
		},
		"another version of the server": {
			breakIt: func(env *testEnv) { env.t.Setenv("FAKE_SERVER_VERSION", "11.4.14-MariaDB-ubu2404") },
			warn:    "mariadb-backup is not of the version of the server",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			c.breakIt(env)

			var out bytes.Buffer
			err := env.app.Check(context.Background(), &out)
			if c.fail != "" {
				if !errors.Is(err, errCheckFailed) || !strings.Contains(out.String(), "FAIL") || !strings.Contains(out.String(), c.fail) {
					t.Errorf("got %v:\n%s", err, out.String())
				}
			}
			if c.warn != "" {
				if err != nil || !strings.Contains(out.String(), "WARN  "+c.warn) {
					t.Errorf("got %v:\n%s", err, out.String())
				}
			}
		})
	}
}

func TestSetupWithoutAConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "etc", "mariadb-backuper.json")

	cfg, err := createDefaultConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != defaultConfig() || loaded != cfg {
		t.Errorf("created %+v, loaded %+v", cfg, loaded)
	}

	// An existing config is never overwritten
	if _, err := createDefaultConfig(path); !errors.Is(err, fs.ErrExist) {
		t.Errorf("got %v, want fs.ErrExist", err)
	}
}

// setup is the only command that creates the backup root
func TestRunSetupCreatesTheRoot(t *testing.T) {
	env := newTestEnv(t)
	cfg := env.app.cfg
	cfg.BackupRoot = filepath.Join(t.TempDir(), "new", "root")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"status", "-config", writeConfig(t, cfg)}, &stdout, &stderr); code != exitFailed {
		t.Errorf("status without the root: exit code %d, want %d", code, exitFailed)
	}
	if code := run([]string{"setup", "-config", writeConfig(t, cfg)}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code %d\n%s%s", code, stdout.String(), stderr.String())
	}
	info, err := os.Stat(cfg.BackupRoot)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != dirMode {
		t.Errorf("the root has mode %v, want %v", info.Mode().Perm(), dirMode)
	}
}

func TestParseAccount(t *testing.T) {
	got, err := parseAccount("odd@user@localhost")
	if err != nil || got != (account{user: "odd@user", host: "localhost"}) {
		t.Errorf("got %+v, %v", got, err)
	}
	if got.sql() != "'odd@user'@'localhost'" {
		t.Errorf("sql: %s", got.sql())
	}
	if quoted := (account{user: `o'b\`, host: "%"}).sql(); quoted != `'o\'b\\'@'%'` {
		t.Errorf("escaping: %s", quoted)
	}
	for _, value := range []string{"", "root", "@localhost", "root@"} {
		if got, err := parseAccount(value); err == nil {
			t.Errorf("%q: parsed %+v", value, got)
		}
	}
}

func TestSameVersions(t *testing.T) {
	backup := "/usr/bin/mariadb-backup based on MariaDB server 11.4.13-MariaDB debian-linux-gnu (x86_64)"
	if same, err := sameVersions(backup, "11.4.13-MariaDB-ubu2404"); err != nil || !same {
		t.Errorf("the same versions: %v, %v", same, err)
	}
	if same, err := sameVersions(backup, "11.4.14-MariaDB-ubu2404"); err != nil || same {
		t.Errorf("different versions: %v, %v", same, err)
	}
	if _, err := sameVersions("mariadb-backup", "11.4.13"); err == nil {
		t.Error("compared a version that is not there")
	}
}
