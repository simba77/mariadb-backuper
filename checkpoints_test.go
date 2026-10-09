package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseCheckpoints(t *testing.T) {
	// The example of the MariaDB documentation
	data := "backup_type = incremental\nfrom_lsn = 1635102\nto_lsn = 1635114\nlast_lsn = 1635114\nrecover_binlog_info = 0\n"
	got, err := parseCheckpoints([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	want := Checkpoints{BackupType: "incremental", FromLSN: 1635102, ToLSN: 1635114, LastLSN: 1635114, HasLastLSN: true}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}

	for _, data := range []string{
		"",
		"backup_type = full-backuped\nfrom_lsn = 0\n",
		"backup_type = full-backuped\nfrom_lsn = 0\nto_lsn = many\n",
		"from_lsn = 0\nto_lsn = 10\n",
		"backup_type full-backuped\nfrom_lsn = 0\nto_lsn = 10\n",
		"backup_type = full-backuped\nfrom_lsn = -1\nto_lsn = 10\n",
	} {
		if checkpoints, err := parseCheckpoints([]byte(data)); err == nil {
			t.Errorf("%q: parsed %+v", data, checkpoints)
		}
	}
}

func TestVerifyCheckpoints(t *testing.T) {
	full := Checkpoints{BackupType: backupTypeFull, ToLSN: 100, LastLSN: 100, HasLastLSN: true}
	if err := full.verify(Full, nil); err != nil {
		t.Errorf("full: %v", err)
	}
	incremental := Checkpoints{BackupType: backupTypeIncremental, FromLSN: 100, ToLSN: 100}
	if err := incremental.verify(Incremental, &full); err != nil {
		t.Errorf("an incremental copy of an idle server: %v", err)
	}

	cases := map[string]struct {
		checkpoints Checkpoints
		typ         CopyType
		base        *Checkpoints
	}{
		"a full copy of the incremental type":  {Checkpoints{BackupType: backupTypeIncremental, ToLSN: 100}, Full, nil},
		"a full copy with from_lsn":            {Checkpoints{BackupType: backupTypeFull, FromLSN: 5, ToLSN: 100}, Full, nil},
		"a full copy without to_lsn":           {Checkpoints{BackupType: backupTypeFull}, Full, nil},
		"a prepared copy":                      {Checkpoints{BackupType: "log-applied", ToLSN: 100}, Full, nil},
		"an incremental copy of the full type": {Checkpoints{BackupType: backupTypeFull, FromLSN: 100, ToLSN: 200}, Incremental, &full},
		"a gap in the LSN":                     {Checkpoints{BackupType: backupTypeIncremental, FromLSN: 150, ToLSN: 200}, Incremental, &full},
		"an overlap in the LSN":                {Checkpoints{BackupType: backupTypeIncremental, FromLSN: 50, ToLSN: 200}, Incremental, &full},
		"to_lsn before from_lsn":               {Checkpoints{BackupType: backupTypeIncremental, FromLSN: 100, ToLSN: 90}, Incremental, &full},
		"no base":                              {Checkpoints{BackupType: backupTypeIncremental, FromLSN: 100, ToLSN: 200}, Incremental, nil},
		"last_lsn before to_lsn":               {Checkpoints{BackupType: backupTypeFull, ToLSN: 100, LastLSN: 90, HasLastLSN: true}, Full, nil},
	}
	for name, c := range cases {
		if err := c.checkpoints.verify(c.typ, c.base); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// MariaDB before 11.1 names the file xtrabackup_checkpoints
func TestReadCheckpointsOfOlderVersions(t *testing.T) {
	dir := t.TempDir()
	data := "backup_type = full-backuped\nfrom_lsn = 0\nto_lsn = 10\n"
	if err := os.WriteFile(filepath.Join(dir, "xtrabackup_checkpoints"), []byte(data), 0640); err != nil {
		t.Fatal(err)
	}
	checkpoints, err := readCheckpoints(dir)
	if err != nil || checkpoints.ToLSN != 10 {
		t.Errorf("got %+v, %v", checkpoints, err)
	}

	if _, err := readCheckpoints(t.TempDir()); err == nil {
		t.Error("an empty directory has checkpoints")
	}
}
