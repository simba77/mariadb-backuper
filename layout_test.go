package main

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// The expressions of the nxsBackup template of the downloader
// (github.com/simba77/backup-downloader, files.go and client.go): the
// published copies must be recognized by them as they are
var (
	downloaderPathExpression = regexp.MustCompile(`(?:^|/)(daily|weekly|monthly)/[^/]+_(\d{4}-\d{2}-\d{2}_\d{2}-\d{2})\.`)
	downloaderExtensions     = map[string]bool{".gz": true, ".tgz": true, ".tar": true, ".bz2": true, ".xz": true, ".zst": true, ".zip": true}
)

// Every copy of a chain must look to the downloader like a copy made when the
// chain started: it deletes them by that date, all at once
func TestCopyNamesMatchTheDownloader(t *testing.T) {
	started := time.Date(2026, 10, 11, 23, 0, 4, 0, time.UTC)
	full := newFullName("mariadb", started)
	incremental := newIncrementalName("mariadb", full.chainID, started.Add(24*time.Hour+2*time.Second))

	for _, name := range []copyName{full, incremental} {
		remote := path.Join("/var/backups/mariadb", statePath(name))
		matches := downloaderPathExpression.FindStringSubmatch(remote)
		if matches == nil {
			t.Errorf("%s: not recognized by the downloader", remote)
			continue
		}
		if matches[1] != periodDir || matches[2] != "2026-10-11_23-00" {
			t.Errorf("%s: the downloader sees period %q and date %q", remote, matches[1], matches[2])
		}
		if created, err := time.Parse("2006-01-02_15-04", matches[2]); err != nil || !created.Equal(started.Truncate(time.Minute)) {
			t.Errorf("%s: the downloader dates it %v, %v", remote, created, err)
		}
		if !downloaderExtensions[path.Ext(remote)] {
			t.Errorf("%s: the extension is not downloaded", remote)
		}
	}

	if full.file() != "mariadb_2026-10-11_23-00.full.xb.zst" {
		t.Errorf("full: %s", full.file())
	}
	if incremental.file() != "mariadb_2026-10-11_23-00.inc-20261012-230006.xb.zst" {
		t.Errorf("incremental: %s", incremental.file())
	}
	// The order of the names is the order of the copies
	if !(full.file() < incremental.file()) {
		t.Error("the full copy does not sort first")
	}
}

func TestParseCopyName(t *testing.T) {
	for _, base := range []string{
		"mariadb_2026-10-11_23-00.full",
		"db-1_2026-10-11_23-00.inc-20261012-230006",
	} {
		name, ok := parseCopyBase(base)
		if !ok || name.base() != base {
			t.Errorf("%s: parsed %+v, %v", base, name, ok)
		}
	}

	for _, base := range []string{
		"",
		"mariadb_2026-10-11_23-00",
		"mariadb_2026-13-11_23-00.full",
		"mariadb_2026-10-11_23-00.inc-20261312-230006",
		"Mariadb_2026-10-11_23-00.full",
		"../mariadb_2026-10-11_23-00.full",
		"mariadb_2026-10-11_23-00.full.xb.zst",
	} {
		if name, ok := parseCopyBase(base); ok {
			t.Errorf("%q: parsed %+v", base, name)
		}
	}
}

func TestParseStatePath(t *testing.T) {
	if _, err := parseStatePath("publish/weekly/mariadb_2026-10-11_23-00.full.xb.zst"); err != nil {
		t.Error(err)
	}
	for _, statePath := range []string{
		"/var/backups/mariadb/publish/weekly/mariadb_2026-10-11_23-00.full.xb.zst",
		"publish/weekly/../weekly/mariadb_2026-10-11_23-00.full.xb.zst",
		"publish/mariadb_2026-10-11_23-00.full.xb.zst",
		"publish/weekly/mariadb_2026-10-11_23-00.full",
		"",
	} {
		if _, err := parseStatePath(statePath); err == nil {
			t.Errorf("%q: accepted", statePath)
		}
	}
}

func TestRemoveInsideRefusesDangerousPaths(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	outside := filepath.Join(dir, "outside")
	for _, path := range []string{filepath.Join(root, "staging"), outside} {
		if err := os.MkdirAll(path, 0750); err != nil {
			t.Fatal(err)
		}
	}
	victim := filepath.Join(outside, "precious")
	if err := os.WriteFile(victim, []byte("data"), 0640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "staging", "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	cases := map[string][2]string{
		"the root itself":            {root, root},
		"outside the root":           {root, outside},
		"a parent of the root":       {root, dir},
		"an escape with dots":        {root, filepath.Join(root, "..", "outside")},
		"through a symlink":          {root, filepath.Join(link, "precious")},
		"a symlink itself":           {root, link},
		"a relative path":            {root, "staging"},
		"an empty path":              {root, ""},
		"the file system root":       {"/", outside},
		"an empty root":              {"", outside},
		"a relative root":            {"root", filepath.Join(root, "staging")},
		"a root that is a substring": {root, root + "-other"},
	}
	for name, paths := range cases {
		t.Run(name, func(t *testing.T) {
			if err := removeInside(paths[0], paths[1]); err == nil {
				t.Errorf("removeInside(%q, %q) succeeded", paths[0], paths[1])
			}
		})
	}

	if _, err := os.Stat(victim); err != nil {
		t.Errorf("the file outside the root is gone: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the symlink is gone: %v", err)
	}

	inside := filepath.Join(root, "staging", "copy")
	if err := os.MkdirAll(filepath.Join(inside, "lsn"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := removeInside(root, inside); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inside); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory inside the root is not removed: %v", err)
	}
}

func TestCalendarDays(t *testing.T) {
	cases := []struct {
		from, to time.Time
		want     int
	}{
		{time.Date(2026, 10, 12, 23, 30, 0, 0, testLocation), time.Date(2026, 10, 19, 0, 30, 0, 0, testLocation), 7},
		{time.Date(2026, 10, 12, 23, 30, 0, 0, testLocation), time.Date(2026, 10, 18, 23, 59, 0, 0, testLocation), 6},
		{time.Date(2026, 10, 12, 2, 0, 0, 0, testLocation), time.Date(2026, 10, 12, 23, 0, 0, 0, testLocation), 0},
		// UTC moments are taken by the local date: 23:00 UTC is the next day in Moscow
		{time.Date(2026, 10, 11, 23, 0, 0, 0, time.UTC), time.Date(2026, 10, 19, 2, 0, 0, 0, testLocation), 7},
		// The clock turned back
		{time.Date(2026, 10, 19, 2, 0, 0, 0, testLocation), time.Date(2026, 10, 18, 2, 0, 0, 0, testLocation), -1},
	}
	for _, c := range cases {
		if got := calendarDays(c.from, c.to, testLocation); got != c.want {
			t.Errorf("calendarDays(%v, %v) = %d, want %d", c.from, c.to, got, c.want)
		}
	}

	// A day with a clock change is still one calendar day
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip(err)
	}
	if got := calendarDays(time.Date(2026, 10, 24, 2, 0, 0, 0, berlin), time.Date(2026, 10, 25, 2, 0, 0, 0, berlin), berlin); got != 1 {
		t.Errorf("across the clock change: got %d, want 1", got)
	}
}
