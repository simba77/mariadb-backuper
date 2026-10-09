package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The checkpoints file is named mariadb_backup_checkpoints since MariaDB 11.1
// and xtrabackup_checkpoints before. Both are accepted, the current name first.
var checkpointsFileNames = []string{"mariadb_backup_checkpoints", "xtrabackup_checkpoints"}

// The backup_type values of copies that have not been prepared
const (
	backupTypeFull        = "full-backuped"
	backupTypeIncremental = "incremental"
)

// Checkpoints is the metadata mariadb-backup writes about a copy:
//
//	backup_type = incremental
//	from_lsn = 1635102
//	to_lsn = 1635114
//	last_lsn = 1635114
type Checkpoints struct {
	BackupType string
	FromLSN    uint64
	ToLSN      uint64
	// LastLSN is optional: the documentation lists it, but the checks do not depend on it
	LastLSN    uint64
	HasLastLSN bool
}

// readCheckpoints reads the checkpoints file from the LSN directory of a copy,
// the copy saved by --extra-lsndir
func readCheckpoints(dir string) (Checkpoints, error) {
	if err := requireRealDir(dir); err != nil {
		return Checkpoints{}, err
	}

	for _, name := range checkpointsFileNames {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Checkpoints{}, err
		}

		checkpoints, err := parseCheckpoints(data)
		if err != nil {
			return Checkpoints{}, fmt.Errorf("%s: %w", filepath.Join(dir, name), err)
		}
		return checkpoints, nil
	}

	return Checkpoints{}, fmt.Errorf("no %s in %s", strings.Join(checkpointsFileNames, " or "), dir)
}

func parseCheckpoints(data []byte) (Checkpoints, error) {
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return Checkpoints{}, fmt.Errorf("malformed line %q", line)
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}

	var errs []error
	lsn := func(key string, required bool) (uint64, bool) {
		value, ok := values[key]
		if !ok {
			if required {
				errs = append(errs, fmt.Errorf("%s is missing", key))
			}
			return 0, false
		}
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s is not a number: %q", key, value))
			return 0, false
		}
		return parsed, true
	}

	checkpoints := Checkpoints{BackupType: values["backup_type"]}
	if checkpoints.BackupType == "" {
		errs = append(errs, errors.New("backup_type is missing"))
	}
	checkpoints.FromLSN, _ = lsn("from_lsn", true)
	checkpoints.ToLSN, _ = lsn("to_lsn", true)
	checkpoints.LastLSN, checkpoints.HasLastLSN = lsn("last_lsn", false)

	return checkpoints, errors.Join(errs...)
}

// verify checks the metadata of a copy of the given type. The base is the
// previous copy of the chain an incremental copy continues.
func (c Checkpoints) verify(typ CopyType, base *Checkpoints) error {
	var errs []error
	switch typ {
	case Full:
		if c.BackupType != backupTypeFull {
			errs = append(errs, fmt.Errorf("backup_type is %q, expected %q for a full copy", c.BackupType, backupTypeFull))
		}
		if c.FromLSN != 0 {
			errs = append(errs, fmt.Errorf("from_lsn of a full copy must be 0, got %d", c.FromLSN))
		}
		if c.ToLSN == 0 {
			errs = append(errs, errors.New("to_lsn of a full copy must be greater than 0"))
		}
	case Incremental:
		if c.BackupType != backupTypeIncremental {
			errs = append(errs, fmt.Errorf("backup_type is %q, expected %q for an incremental copy", c.BackupType, backupTypeIncremental))
		}
		if base == nil {
			errs = append(errs, errors.New("an incremental copy has no previous copy to continue"))
		} else if c.FromLSN != base.ToLSN {
			errs = append(errs, fmt.Errorf("from_lsn %d does not continue to_lsn %d of the previous copy", c.FromLSN, base.ToLSN))
		}
		if c.ToLSN < c.FromLSN {
			errs = append(errs, fmt.Errorf("to_lsn %d is less than from_lsn %d", c.ToLSN, c.FromLSN))
		}
	default:
		errs = append(errs, fmt.Errorf("unknown copy type %q", typ))
	}
	if c.HasLastLSN && c.LastLSN < c.ToLSN {
		errs = append(errs, fmt.Errorf("last_lsn %d is less than to_lsn %d", c.LastLSN, c.ToLSN))
	}
	return errors.Join(errs...)
}
