package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig saves the config to a file for the tests that go through run
func writeConfig(t *testing.T, cfg Config) string {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0640); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigKeepsTheDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"backupRoot": "/srv/backups/", "zstdThreads": 0}`), 0640); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := defaultConfig()
	want.BackupRoot = "/srv/backups"
	want.ZstdThreads = 0
	if cfg != want {
		t.Errorf("got %+v\nwant %+v", cfg, want)
	}
}

// The example in the repository must stay loadable
func TestConfigExample(t *testing.T) {
	cfg, err := loadConfig("config.json.example")
	if err != nil {
		t.Fatal(err)
	}
	if cfg != defaultConfig() {
		t.Errorf("the example differs from the defaults:\ngot  %+v\nwant %+v", cfg, defaultConfig())
	}
}

func TestLoadConfigReportsEveryMistake(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	config := `{
  "backupRoot": "/",
  "name": "Maria_DB",
  "credentialsFile": "mariadb-backup.cnf",
  "mariadbBackupPath": "",
  "zstdPath": "",
  "fullIntervalDays": 0,
  "minFreeSpaceGB": -1,
  "zstdLevel": 22,
  "zstdThreads": -1,
  "parallel": 0,
  "keepLogs": 0,
  "logLevel": "verbose"
}`
	if err := os.WriteFile(path, []byte(config), 0640); err != nil {
		t.Fatal(err)
	}

	_, err := loadConfig(path)
	if err == nil {
		t.Fatal("the config is accepted")
	}
	for _, key := range []string{"backupRoot", "name", "credentialsFile", "mariadbBackupPath", "zstdPath", "fullIntervalDays", "minFreeSpaceGB", "zstdLevel", "zstdThreads", "parallel", "keepLogs", "logLevel"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("no error about %s in:\n%v", key, err)
		}
	}
}

func TestLoadConfigRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"fullIntervalDay": 3}`), 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "fullIntervalDay") {
		t.Errorf("got %v", err)
	}
}
