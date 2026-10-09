package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// credentialsHeader marks a credentials file written by setup, so the package
// removes on purge only the file it made
const credentialsHeader = "# Written by mariadb-backuper setup: the account of the backups"

// Setup prepares the server for the backups: the MariaDB account, its
// credentials file, the privileges and the directories of the backup root,
// then runs the check. Every step looks
// first and changes only what is missing or broken, so it runs on every
// package upgrade without touching a working setup.
func (a *App) Setup(ctx context.Context, w io.Writer) error {
	for _, program := range []struct{ path, pkg string }{
		{a.cfg.MariadbBackupPath, "mariadb-backup"},
		{a.cfg.ZstdPath, "zstd"},
		{a.cfg.MariadbPath, "mariadb-client"},
	} {
		if _, err := exec.LookPath(program.path); err != nil {
			return fmt.Errorf("%s is not found, install the %s package: %w", program.path, program.pkg, err)
		}
	}

	version, err := a.queryRow(ctx, "", "SELECT VERSION();")
	if err != nil {
		return fmt.Errorf("cannot connect to MariaDB as the OS user through the socket, run the setup as root while the server is running: %w", err)
	}
	socketAccount, err := a.queryRow(ctx, "", "SELECT CURRENT_USER();")
	if err != nil {
		return err
	}
	a.log.Info("connected to MariaDB", "version", version, "account", socketAccount)

	backupAccount, err := a.ensureCredentials(ctx, socketAccount)
	if err != nil {
		return err
	}
	if err := a.grantPrivileges(ctx, backupAccount); err != nil {
		return err
	}
	if err := a.layout.ensureDirs(); err != nil {
		return err
	}

	a.log.Info("the setup is done, checking everything")
	return a.Check(ctx, w)
}

// ensureCredentials keeps a credentials file that logs in, and otherwise sets
// a new random password for the backup account, creating it when needed.
// The account is a dedicated one, so resetting its password breaks nothing
// but the file that did not work anyway.
func (a *App) ensureCredentials(ctx context.Context, socketAccount string) (account, error) {
	path := a.cfg.CredentialsFile
	if _, err := os.Lstat(path); err == nil {
		if err := a.secureCredentialsFile(path); err != nil {
			return account{}, err
		}
		current, err := a.loginWithCredentials(ctx, socketAccount)
		if err == nil {
			existing, parseErr := parseAccount(current)
			if parseErr == nil {
				a.log.Info("the credentials file logs in, keeping it", "path", path, "account", existing)
				return existing, nil
			}
			err = parseErr
		}
		a.log.Warn("the credentials file does not log in, setting a new password", "path", path, "error", err)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return account{}, fmt.Errorf("credentials file: %w", err)
	}

	backupAccount := account{user: backupUser, host: backupHost}
	exists, err := a.queryRow(ctx, "", fmt.Sprintf("SELECT COUNT(*) FROM mysql.user WHERE User=%s AND Host=%s;", quoteSQL(backupAccount.user), quoteSQL(backupAccount.host)))
	if err != nil {
		return account{}, err
	}

	// 128 random bits in base32: nothing to escape in SQL or in the option file
	password := rand.Text()
	statements := []string{
		fmt.Sprintf("CREATE USER IF NOT EXISTS %s IDENTIFIED BY %s;", backupAccount.sql(), quoteSQL(password)),
		fmt.Sprintf("ALTER USER %s IDENTIFIED BY %s;", backupAccount.sql(), quoteSQL(password)),
	}
	if _, err := a.query(ctx, "", statements...); err != nil {
		// The client may quote the failed statement, the password included
		return account{}, errors.New(strings.ReplaceAll(err.Error(), password, "<password>"))
	}

	// Written after the password is set: a failure here leaves an account
	// with a password nobody knows, and the next setup sets a new one
	// [client] is read by both mariadb-backup and the mariadb client of the
	// checks, while the client ignores [mariadb-backup]
	content := fmt.Sprintf("%s\n[client]\nuser=%s\npassword=%s\n", credentialsHeader, backupAccount.user, password)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return account{}, err
	}
	if err := writeFileAtomic(path, []byte(content), 0600); err != nil {
		return account{}, fmt.Errorf("the password of %s is set, but the credentials file was not written (run the setup again): %w", backupAccount, err)
	}

	if exists == "0" {
		a.log.Info("created the backup account with a random password", "account", backupAccount, "credentials", path)
	} else {
		a.log.Info("set a new random password for the backup account", "account", backupAccount, "credentials", path)
	}

	current, err := a.loginWithCredentials(ctx, socketAccount)
	if err != nil {
		return account{}, fmt.Errorf("the new credentials file does not log in: %w", err)
	}
	if current != backupAccount.String() {
		return account{}, fmt.Errorf("the new credentials file logs in as %s instead of %s", current, backupAccount)
	}
	return backupAccount, nil
}

// secureCredentialsFile closes a credentials file to everyone but its owner, root
func (a *App) secureCredentialsFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("the credentials file %s is not a regular file", path)
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	wrongOwner := ok && int(stat.Uid) != os.Geteuid()
	if info.Mode().Perm()&0077 == 0 && !wrongOwner {
		return nil
	}
	a.log.Warn("the credentials file is open to other users, making it private", "path", path, "mode", info.Mode().Perm())
	if err := os.Chown(path, os.Geteuid(), os.Getegid()); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

// grantPrivileges grants what the backups need and points out the rest
func (a *App) grantPrivileges(ctx context.Context, backupAccount account) error {
	grant := fmt.Sprintf("GRANT %s ON *.* TO %s;", strings.Join(backupPrivileges, ", "), backupAccount.sql())
	if _, err := a.query(ctx, "", grant); err != nil {
		return err
	}

	grants, err := a.query(ctx, "", fmt.Sprintf("SHOW GRANTS FOR %s;", backupAccount.sql()))
	if err != nil {
		return err
	}
	a.log.Info("granted the backup privileges", "account", backupAccount, "privileges", strings.Join(backupPrivileges, ", "))
	for _, extra := range extraGrants(grants) {
		a.log.Warn("the account has more than the backups need, consider revoking it", "account", backupAccount, "grant", extra)
	}
	return nil
}

// createDefaultConfig writes the default config when there is none yet, for a
// setup made without the package, which brings its own
func createDefaultConfig(path string) (Config, error) {
	cfg := defaultConfig()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return Config{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return Config{}, err
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode)
	if err != nil {
		return Config{}, err
	}
	_, err = file.Write(append(data, '\n'))
	return cfg, errors.Join(err, file.Close())
}
