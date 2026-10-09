package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

type App struct {
	cfg    Config
	layout layout
	log    *slog.Logger
	// now, location and freeSpace are replaced in the tests
	now       func() time.Time
	location  *time.Location
	freeSpace func(path string) (uint64, error)
	// The commands found by preflight
	mariadbBackup string
	zstd          string
}

func newApp(cfg Config, log *slog.Logger) (*App, error) {
	// The root is resolved once: the checks of the paths below it compare
	// with the real directory, and none of them may be a symlink
	root, err := filepath.EvalSymlinks(cfg.BackupRoot)
	if err != nil {
		return nil, fmt.Errorf("backup root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("backup root: %w", err)
	}
	if !info.IsDir() || root == "/" {
		return nil, fmt.Errorf("backup root %s must be a directory other than /", root)
	}

	return &App{
		cfg:       cfg,
		layout:    layout{root: root},
		log:       log,
		now:       time.Now,
		location:  time.Local,
		freeSpace: freeSpace,
	}, nil
}

// preflight checks everything a backup needs before anything is changed
func (a *App) preflight(ctx context.Context) error {
	var err error
	if a.mariadbBackup, err = exec.LookPath(a.cfg.MariadbBackupPath); err != nil {
		return fmt.Errorf("mariadb-backup is not found (mariadbBackupPath %q), install the mariadb-backup package: %w", a.cfg.MariadbBackupPath, err)
	}
	if a.zstd, err = exec.LookPath(a.cfg.ZstdPath); err != nil {
		return fmt.Errorf("zstd is not found (zstdPath %q), install the zstd package: %w", a.cfg.ZstdPath, err)
	}
	if err := checkCredentialsFile(a.cfg.CredentialsFile); err != nil {
		return err
	}
	if err := a.layout.ensureDirs(); err != nil {
		return err
	}

	version, err := mariadbBackupVersion(ctx, a.mariadbBackup)
	if err != nil {
		return err
	}
	a.log.Info("found mariadb-backup", "path", a.mariadbBackup, "version", version)
	return nil
}

// checkCredentialsFile makes sure the file with the password exists, is
// readable and is not open to other users. Its content is never read: it is
// up to mariadb-backup.
func checkCredentialsFile(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the credentials file %s does not exist, create it as described in the README", path)
	}
	if err != nil {
		return fmt.Errorf("credentials file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("the credentials file %s is not a regular file", path)
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("the credentials file %s holds a password and must be accessible by its owner only (chmod 600), got %v", path, info.Mode().Perm())
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != 0 && int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("the credentials file %s must be owned by root or by the user running the backup, got uid %d", path, stat.Uid)
	}

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("credentials file: %w", err)
	}
	return file.Close()
}
