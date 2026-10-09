package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
)

const defaultConfigPath = "/etc/mariadb-backuper.json"

type Config struct {
	// BackupRoot holds the published copies, the staging area, the state and the logs
	BackupRoot string `json:"backupRoot"`
	// Name is the prefix of the copy file names
	Name string `json:"name"`
	// CredentialsFile is an option file with the user and the password for
	// mariadb-backup, passed with --defaults-extra-file
	CredentialsFile string `json:"credentialsFile"`
	// The commands are looked up in PATH unless they are paths. The mariadb
	// client is used by setup and check only.
	MariadbBackupPath string `json:"mariadbBackupPath"`
	ZstdPath          string `json:"zstdPath"`
	MariadbPath       string `json:"mariadbPath"`
	// FullIntervalDays is how many calendar days a full copy starts a chain for
	FullIntervalDays int `json:"fullIntervalDays"`
	// MinFreeSpaceGB is the free space that must be left on top of the expected copy size
	MinFreeSpaceGB int `json:"minFreeSpaceGB"`
	ZstdLevel      int `json:"zstdLevel"`
	// ZstdThreads is passed to zstd -T, 0 uses every core
	ZstdThreads int `json:"zstdThreads"`
	// Parallel is the number of threads mariadb-backup copies the data files with
	Parallel int `json:"parallel"`
	// KeepLogs is how many logs of mariadb-backup runs are kept in BackupRoot/logs
	KeepLogs int    `json:"keepLogs"`
	LogLevel string `json:"logLevel"`
}

func defaultConfig() Config {
	return Config{
		BackupRoot:        "/var/backups/mariadb",
		Name:              "mariadb",
		CredentialsFile:   "/etc/mysql/mariadb-backup.cnf",
		MariadbBackupPath: "mariadb-backup",
		ZstdPath:          "zstd",
		MariadbPath:       "mariadb",
		FullIntervalDays:  7,
		MinFreeSpaceGB:    20,
		ZstdLevel:         3,
		ZstdThreads:       2,
		Parallel:          1,
		KeepLogs:          30,
		LogLevel:          "info",
	}
}

// The name goes into the copy file names before the date the downloader
// parses, so it is kept to characters that can never be taken for a date part
var nameExpression = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// loadConfig reads the config file over the defaults: a key left out keeps its
// default value, an unknown key is an error rather than a silently ignored typo
func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	config := defaultConfig()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("invalid JSON: unexpected data after the config object")
	}

	if err := validateConfig(config); err != nil {
		return Config{}, err
	}
	config.BackupRoot = filepath.Clean(config.BackupRoot)
	return config, nil
}

// validateConfig reports every mistake in the config at once
func validateConfig(config Config) error {
	var errs []error
	if !filepath.IsAbs(config.BackupRoot) || filepath.Clean(config.BackupRoot) == "/" {
		errs = append(errs, fmt.Errorf("backupRoot must be an absolute path other than /, got %q", config.BackupRoot))
	}
	if !nameExpression.MatchString(config.Name) {
		errs = append(errs, fmt.Errorf("name must consist of lowercase letters, digits and dashes, got %q", config.Name))
	}
	if !filepath.IsAbs(config.CredentialsFile) {
		errs = append(errs, fmt.Errorf("credentialsFile must be an absolute path, got %q", config.CredentialsFile))
	}
	if config.MariadbBackupPath == "" {
		errs = append(errs, errors.New("mariadbBackupPath must not be empty"))
	}
	if config.ZstdPath == "" {
		errs = append(errs, errors.New("zstdPath must not be empty"))
	}
	if config.MariadbPath == "" {
		errs = append(errs, errors.New("mariadbPath must not be empty"))
	}
	if config.FullIntervalDays < 1 {
		errs = append(errs, fmt.Errorf("fullIntervalDays must be at least 1, got %d", config.FullIntervalDays))
	}
	if config.MinFreeSpaceGB < 0 {
		errs = append(errs, fmt.Errorf("minFreeSpaceGB must not be negative, got %d", config.MinFreeSpaceGB))
	}
	// Levels above 19 need --ultra and a lot of memory
	if config.ZstdLevel < 1 || config.ZstdLevel > 19 {
		errs = append(errs, fmt.Errorf("zstdLevel must be from 1 to 19, got %d", config.ZstdLevel))
	}
	if config.ZstdThreads < 0 {
		errs = append(errs, fmt.Errorf("zstdThreads must not be negative, got %d", config.ZstdThreads))
	}
	if config.Parallel < 1 {
		errs = append(errs, fmt.Errorf("parallel must be at least 1, got %d", config.Parallel))
	}
	if config.KeepLogs < 1 {
		errs = append(errs, fmt.Errorf("keepLogs must be at least 1, got %d", config.KeepLogs))
	}
	if _, ok := logLevels[config.LogLevel]; !ok {
		errs = append(errs, fmt.Errorf("logLevel must be one of debug, info, warn, error, got %q", config.LogLevel))
	}
	return errors.Join(errs...)
}
