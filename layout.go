package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The layout of the backup root. Only publish/ is read by the downloader, it is
// its backupsPath: nothing gets there before it has been verified. Everything
// else is the own data of the backuper.
const (
	publishDir    = "publish"
	periodDir     = "weekly"
	stagingDir    = "staging"
	lsnDir        = "lsn"
	logsDir       = "logs"
	stateFileName = "state.json"
	lockFileName  = "lock"
)

const (
	archiveExt = ".xb.zst"
	partSuffix = ".part"
	logExt     = ".log"
)

// The copies hold the whole database: other local users get no access
const (
	dirMode  os.FileMode = 0750
	fileMode os.FileMode = 0640
)

type CopyType string

const (
	Full        CopyType = "full"
	Incremental CopyType = "incremental"
)

// The copy names follow the nxs-backup layout the downloader understands,
// <period>/<name>_YYYY-MM-DD_HH-MM.<ext>, where the date is the start of the
// chain for every copy of it:
//
//	mariadb_2026-10-12_23-00.full.xb.zst
//	mariadb_2026-10-12_23-00.inc-20261013-230002.xb.zst
//
// The downloader takes the age of a file from that date, so all the copies of
// a chain expire together: an incremental copy is useless without the full
// copy and every incremental copy before it. The start of the incremental copy
// itself goes after the dot, where the downloader does not look. The times are
// UTC, the downloader parses them as UTC.
const (
	chainIDLayout = "2006-01-02_15-04"
	incTagLayout  = "20060102-150405"
)

var copyNameExpression = regexp.MustCompile(`^([a-z0-9][a-z0-9-]*)_(\d{4}-\d{2}-\d{2}_\d{2}-\d{2})\.(full|inc-\d{8}-\d{6})$`)

// copyName identifies a copy, its archive and its LSN directory
type copyName struct {
	prefix  string
	chainID string
	typ     CopyType
	// started is when the copy was started, UTC, to the minute for a full copy
	started time.Time
}

func newFullName(prefix string, started time.Time) copyName {
	started = started.UTC().Truncate(time.Minute)
	return copyName{prefix: prefix, chainID: started.Format(chainIDLayout), typ: Full, started: started}
}

func newIncrementalName(prefix, chainID string, started time.Time) copyName {
	return copyName{prefix: prefix, chainID: chainID, typ: Incremental, started: started.UTC().Truncate(time.Second)}
}

// base is the name without the archive extension, also the name of the LSN directory
func (n copyName) base() string {
	if n.typ == Full {
		return n.prefix + "_" + n.chainID + ".full"
	}
	return n.prefix + "_" + n.chainID + ".inc-" + n.started.Format(incTagLayout)
}

func (n copyName) file() string {
	return n.base() + archiveExt
}

func parseCopyBase(base string) (copyName, bool) {
	matches := copyNameExpression.FindStringSubmatch(base)
	if matches == nil {
		return copyName{}, false
	}

	chainStart, err := time.Parse(chainIDLayout, matches[2])
	if err != nil {
		return copyName{}, false
	}
	if matches[3] == "full" {
		return copyName{prefix: matches[1], chainID: matches[2], typ: Full, started: chainStart}, true
	}

	started, err := time.Parse(incTagLayout, strings.TrimPrefix(matches[3], "inc-"))
	if err != nil {
		return copyName{}, false
	}
	return copyName{prefix: matches[1], chainID: matches[2], typ: Incremental, started: started}, true
}

func parseCopyFile(file string) (copyName, bool) {
	base, ok := strings.CutSuffix(file, archiveExt)
	if !ok {
		return copyName{}, false
	}
	return parseCopyBase(base)
}

// statePath is the path of a copy as the state records it, relative to the root
func statePath(name copyName) string {
	return path.Join(publishDir, periodDir, name.file())
}

func parseStatePath(statePath string) (copyName, error) {
	dir, file := path.Split(statePath)
	if dir != publishDir+"/"+periodDir+"/" {
		return copyName{}, fmt.Errorf("%q is not a path of a copy, expected %s/%s/<copy>", statePath, publishDir, periodDir)
	}
	name, ok := parseCopyFile(file)
	if !ok {
		return copyName{}, fmt.Errorf("%q is not a copy file name", statePath)
	}
	return name, nil
}

// layout builds the paths inside the backup root. The root is resolved from
// symlinks once, every path below it is checked not to be a symlink.
type layout struct {
	root string
}

func (l layout) publish() string { return filepath.Join(l.root, publishDir, periodDir) }
func (l layout) staging() string { return filepath.Join(l.root, stagingDir) }
func (l layout) lsn() string     { return filepath.Join(l.root, lsnDir) }
func (l layout) logs() string    { return filepath.Join(l.root, logsDir) }
func (l layout) state() string   { return filepath.Join(l.root, stateFileName) }
func (l layout) lock() string    { return filepath.Join(l.root, lockFileName) }

func (l layout) archive(name copyName) string {
	return filepath.Join(l.publish(), name.file())
}

func (l layout) lsnOf(name copyName) string {
	return filepath.Join(l.lsn(), name.base())
}

func (l layout) logOf(name copyName) string {
	return filepath.Join(l.logs(), name.base()+logExt)
}

// ensureDirs creates the directories of the layout that do not exist yet
func (l layout) ensureDirs() error {
	for _, dir := range []string{filepath.Join(l.root, publishDir), l.publish(), l.staging(), l.lsn(), l.logs()} {
		if err := os.Mkdir(dir, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err := requireRealDir(dir); err != nil {
			return err
		}
	}
	return nil
}

// requireRealDir fails unless the path is a directory itself, not a symlink to one
func requireRealDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s must be a directory, not a symlink or a file", dir)
	}
	return nil
}

// readDirIfExists lists a directory of the layout that is created on the first backup
func readDirIfExists(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return entries, err
}

// removeInside removes the target recursively after making sure it lies
// strictly inside the root and no path element below the root, the target
// included, is a symlink: a link planted in the backup root must never lead
// the deletion out of it. os.RemoveAll itself does not follow the symlinks
// it meets inside the target.
func removeInside(root, target string) error {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) == "/" {
		return fmt.Errorf("refusing to delete %q: the root %q is not a safe directory", target, root)
	}
	if !filepath.IsAbs(target) {
		return fmt.Errorf("refusing to delete %q: not an absolute path", target)
	}
	root, target = filepath.Clean(root), filepath.Clean(target)

	relative, err := filepath.Rel(root, target)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refusing to delete %q: it is not inside %q", target, root)
	}

	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to delete %q: %q is a symlink", target, current)
		}
	}

	return os.RemoveAll(target)
}

// syncDir flushes a directory, so the renames in it survive a power loss
func syncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}
