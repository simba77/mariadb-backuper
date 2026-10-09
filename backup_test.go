package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 02:00 in Moscow is 23:00 UTC of the day before: the chain is named in UTC
const (
	firstChain = "2026-10-11_23-00"
	firstFull  = "mariadb_" + firstChain + ".full.xb.zst"
)

func TestFirstBackupIsFull(t *testing.T) {
	env := newTestEnv(t)
	env.mustBackup()

	expectNames(t, "published", env.published(), []string{firstFull})
	env.expectNothingStaged()

	state := env.state()
	want := ChainState{
		ID:             firstChain,
		FullStartedAt:  env.now.UTC(),
		FullPath:       "publish/weekly/" + firstFull,
		LastPath:       "publish/weekly/" + firstFull,
		LastType:       Full,
		LastFromLSN:    0,
		LastToLSN:      100,
		LastStartedAt:  env.now.UTC(),
		LastFinishedAt: env.now.UTC(),
	}
	if state.Chain == nil || *state.Chain != want {
		t.Errorf("state:\ngot  %+v\nwant %+v", state.Chain, want)
	}

	content, err := os.ReadFile(filepath.Join(env.app.layout.publish(), firstFull))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "stream of full-backuped from 0 to 100\n" {
		t.Errorf("archive: got %q", content)
	}

	calls := env.callArgs()
	if len(calls) != 1 {
		t.Fatalf("got %d runs of mariadb-backup, want 1", len(calls))
	}
	// mariadb-backup reads the option files only from the first argument,
	// and the password never appears among the arguments
	if !strings.HasPrefix(calls[0][0], "--defaults-extra-file=") {
		t.Errorf("the first argument is %q, want --defaults-extra-file", calls[0][0])
	}
	for _, arg := range calls[0] {
		if strings.Contains(arg, "secret") || strings.HasPrefix(arg, "--password") || strings.HasPrefix(arg, "--incremental") {
			t.Errorf("unexpected argument %q", arg)
		}
	}

	if _, err := os.Stat(filepath.Join(env.app.layout.logs(), "mariadb_"+firstChain+".full.log")); err != nil {
		t.Errorf("the log of the run: %v", err)
	}
}

func TestIncrementalCopiesContinueTheChain(t *testing.T) {
	env := newTestEnv(t)
	env.mustBackup()

	want := []string{firstFull}
	previous := "mariadb_" + firstChain + ".full"
	for day := 1; day <= 6; day++ {
		env.nextDay()
		env.mustBackup()

		if got, want := env.lastCallArg("--incremental-basedir="), filepath.Join(env.app.layout.lsn(), previous); got != want {
			t.Errorf("day %d: --incremental-basedir=%q, want %q", day, got, want)
		}

		previous = "mariadb_" + firstChain + ".inc-" + env.now.UTC().Format(incTagLayout)
		want = append(want, previous+archiveExt)
		state := env.state()
		if state.Chain.ID != firstChain || state.Chain.LastType != Incremental || state.Chain.LastPath != "publish/weekly/"+previous+archiveExt {
			t.Errorf("day %d: state %+v", day, state.Chain)
		}
		if state.Chain.LastFromLSN != uint64(day*100) || state.Chain.LastToLSN != uint64(day*100+100) {
			t.Errorf("day %d: LSN %d-%d", day, state.Chain.LastFromLSN, state.Chain.LastToLSN)
		}
	}
	expectNames(t, "published", env.published(), want)
}

// The full copy is renewed by calendar days of the server: the time of the
// day it was made does not matter
func TestRotationCountsCalendarDays(t *testing.T) {
	env := newTestEnv(t)
	env.at(2026, 10, 12, 23, 30)
	env.mustBackup()
	oldFull := env.published()[0]

	// The sixth calendar day, a minute before midnight: still the same chain
	env.at(2026, 10, 18, 23, 59)
	env.mustBackup()
	if got := len(env.published()); got != 2 {
		t.Fatalf("got %d copies after the incremental one, want 2", got)
	}

	// The seventh calendar day, though only 6 days and an hour have passed
	env.at(2026, 10, 19, 0, 30)
	env.mustBackup()

	newChain := env.now.UTC().Format(chainIDLayout)
	expectNames(t, "published", env.published(), []string{"mariadb_" + newChain + ".full.xb.zst"})
	expectNames(t, "lsn", env.list(env.app.layout.lsn()), []string{"mariadb_" + newChain + ".full"})
	if state := env.state(); state.Chain.ID != newChain || state.Chain.LastType != Full {
		t.Errorf("state: %+v", state.Chain)
	}
	if env.lastCallArg("--incremental-basedir=") != "" {
		t.Error("the new full copy was made as an incremental one")
	}
	if oldFull == env.published()[0] {
		t.Error("the old full copy is still there")
	}
}

func TestFailedBackupKeepsTheState(t *testing.T) {
	cases := map[string]map[string]string{
		"mariadb-backup fails":             {"FAKE_BACKUP_FAIL": "1"},
		"zstd fails":                       {"FAKE_ZSTD_FAIL": "1"},
		"the archive is corrupt":           {"FAKE_ZSTD_TEST_FAIL": "1"},
		"no checkpoints":                   {"FAKE_CHECKPOINTS": "missing"},
		"garbage checkpoints":              {"FAKE_CHECKPOINTS": "garbage"},
		"the LSN do not continue":          {"FAKE_FROM_LSN": "42"},
		"mariadb-backup succeeds silently": {"FAKE_SLEEP": "0"},
	}
	for name, variables := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			env.mustBackup()
			before := env.stateBytes()

			for key, value := range variables {
				t.Setenv(key, value)
			}
			env.nextDay()
			if err := env.backup(); err == nil {
				t.Fatal("the backup succeeded")
			}

			if !bytes.Equal(env.stateBytes(), before) {
				t.Errorf("the state changed:\n%s", env.stateBytes())
			}
			expectNames(t, "published", env.published(), []string{firstFull})
			expectNames(t, "lsn", env.list(env.app.layout.lsn()), []string{"mariadb_" + firstChain + ".full"})
			env.expectNothingStaged()
		})
	}
}

func TestFailedFirstFullPublishesNothing(t *testing.T) {
	env := newTestEnv(t)
	t.Setenv("FAKE_CHECKPOINTS", "missing")
	if err := env.backup(); err == nil {
		t.Fatal("the backup succeeded")
	}
	if got := env.published(); len(got) > 0 {
		t.Errorf("published %v", got)
	}
	if state := env.stateBytes(); state != nil {
		t.Errorf("the state was written: %s", state)
	}
	env.expectNothingStaged()
}

func TestLockedRunDoesNothing(t *testing.T) {
	env := newTestEnv(t)
	lock, err := acquireLock(env.app.layout.lock())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.release()

	if err := env.backup(); !errors.Is(err, errLocked) {
		t.Fatalf("got %v, want errLocked", err)
	}
	if calls := env.callArgs(); len(calls) > 0 {
		t.Errorf("mariadb-backup was run: %v", calls)
	}
	if code := run([]string{"backup", "-config", writeConfig(t, env.app.cfg)}, os.Stdout, os.Stderr); code != exitLocked {
		t.Errorf("exit code %d, want %d", code, exitLocked)
	}
}

func TestCorruptStateIsRebuiltFromAValidChain(t *testing.T) {
	env := newTestEnv(t)
	env.mustBackup()
	env.nextDay()
	env.mustBackup()

	env.writeState(`{"version": 1, "chain": {"id": `)
	env.nextDay()
	env.mustBackup()

	state := env.state()
	if state.Chain.ID != firstChain || state.Chain.LastFromLSN != 200 || len(env.published()) != 3 {
		t.Errorf("state %+v, published %v", state.Chain, env.published())
	}
	if kept, _ := filepath.Glob(env.app.layout.state() + ".corrupt-*"); len(kept) != 1 {
		t.Errorf("the corrupt state is not kept: %v", kept)
	}
}

func TestMissingStateIsRebuiltFromAValidChain(t *testing.T) {
	env := newTestEnv(t)
	env.mustBackup()
	env.nextDay()
	env.mustBackup()

	if err := os.Remove(env.app.layout.state()); err != nil {
		t.Fatal(err)
	}
	env.nextDay()
	env.mustBackup()

	if state := env.state(); state.Chain.ID != firstChain || state.Chain.LastFromLSN != 200 {
		t.Errorf("state %+v", state.Chain)
	}
}

// A chain that cannot be trusted is never continued: the run stops before
// mariadb-backup with an error that tells what to do
func TestInconsistentChainStopsTheBackups(t *testing.T) {
	cases := map[string]func(env *testEnv){
		"the state contradicts the LSN": func(env *testEnv) {
			state := env.state()
			state.Chain.LastToLSN = 150
			if err := writeState(env.app.layout.state(), state); err != nil {
				env.t.Fatal(err)
			}
		},
		"the LSN of the copies do not continue": func(env *testEnv) {
			lsn := filepath.Join(env.app.layout.lsn(), env.published()[1][:len(env.published()[1])-len(archiveExt)])
			checkpoints := "backup_type = incremental\nfrom_lsn = 50\nto_lsn = 200\n"
			if err := os.WriteFile(filepath.Join(lsn, "mariadb_backup_checkpoints"), []byte(checkpoints), 0640); err != nil {
				env.t.Fatal(err)
			}
		},
		"the full copy is gone": func(env *testEnv) {
			if err := os.Remove(filepath.Join(env.app.layout.publish(), firstFull)); err != nil {
				env.t.Fatal(err)
			}
		},
		"the corrupt state and a broken chain": func(env *testEnv) {
			env.writeState("garbage")
			if err := os.RemoveAll(filepath.Join(env.app.layout.lsn(), "mariadb_"+firstChain+".full")); err != nil {
				env.t.Fatal(err)
			}
		},
		"the state lags behind by two copies": func(env *testEnv) {
			state := env.state()
			full := *state.Chain
			full.LastPath, full.LastType, full.LastFromLSN, full.LastToLSN = full.FullPath, Full, 0, 100
			env.nextDay()
			env.mustBackup()
			if err := writeState(env.app.layout.state(), &State{Version: stateVersion, Chain: &full}); err != nil {
				env.t.Fatal(err)
			}
		},
		"the state records no chain": func(env *testEnv) {
			env.writeState(`{"version": 1, "chain": null}`)
		},
	}
	for name, breakChain := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			env.mustBackup()
			env.nextDay()
			env.mustBackup()
			breakChain(env)
			calls := len(env.callArgs())
			published := env.published()

			env.nextDay()
			err := env.backup()
			if !errors.Is(err, errInconsistent) {
				t.Fatalf("got %v, want errInconsistent", err)
			}
			if got := len(env.callArgs()); got != calls {
				t.Error("mariadb-backup was run")
			}
			expectNames(t, "published", env.published(), published)

			var out bytes.Buffer
			if err := env.app.Check(context.Background(), &out); !errors.Is(err, errCheckFailed) {
				t.Errorf("check: got %v, want errCheckFailed\n%s", err, out.String())
			}
		})
	}
}

func TestNotEnoughSpace(t *testing.T) {
	t.Run("before a copy", func(t *testing.T) {
		env := newTestEnv(t)
		env.free = 10 * gib
		if err := env.backup(); err == nil || !strings.Contains(err.Error(), "not enough free space") {
			t.Fatalf("got %v", err)
		}
		if calls := env.callArgs(); len(calls) > 0 {
			t.Error("mariadb-backup was run")
		}
	})

	// The chain is not deleted for a full copy that has no room anyway
	t.Run("before a rotation", func(t *testing.T) {
		env := newTestEnv(t)
		env.mustBackup()
		before, published := env.stateBytes(), env.published()

		env.free = 1 * gib
		env.at(2026, 10, 19, 2, 0)
		if err := env.backup(); err == nil || !strings.Contains(err.Error(), "not enough free space") {
			t.Fatalf("got %v", err)
		}
		expectNames(t, "published", env.published(), published)
		if !bytes.Equal(env.stateBytes(), before) {
			t.Error("the state changed")
		}
	})
}

func TestLeftoversOfInterruptedRunsAreRemoved(t *testing.T) {
	env := newTestEnv(t)
	stage := filepath.Join(env.app.layout.staging(), "mariadb_"+firstChain+".full")
	if err := os.MkdirAll(filepath.Join(stage, "lsn"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, firstFull+partSuffix), []byte("half of a stream"), 0640); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(env.app.layout.root, ".state.json.123.tmp")
	if err := os.WriteFile(temp, []byte("{"), 0640); err != nil {
		t.Fatal(err)
	}

	env.mustBackup()

	env.expectNothingStaged()
	if _, err := os.Stat(temp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the temporary state is left: %v", err)
	}
	expectNames(t, "published", env.published(), []string{firstFull})
}

// A copy published by a run that failed to write the state is verified
// already: the next run takes it into the state and continues from it
func TestFailedStateWriteIsPickedUpByTheNextRun(t *testing.T) {
	env := newTestEnv(t)
	env.mustBackup()
	before := env.stateBytes()

	renameFile = func(string, string) error { return errors.New("disk failure") }
	t.Cleanup(func() { renameFile = os.Rename })
	env.nextDay()
	if err := env.backup(); err == nil || !strings.Contains(err.Error(), "disk failure") {
		t.Fatalf("got %v", err)
	}
	if !bytes.Equal(env.stateBytes(), before) {
		t.Error("the state changed")
	}
	if temps, _ := filepath.Glob(filepath.Join(env.app.layout.root, ".state.json.*.tmp")); len(temps) > 0 {
		t.Errorf("temporary files are left: %v", temps)
	}
	if got := len(env.published()); got != 2 {
		t.Fatalf("got %d copies, want the full and the incremental one", got)
	}

	renameFile = os.Rename
	env.nextDay()
	env.mustBackup()

	state := env.state()
	if len(env.published()) != 3 || state.Chain.LastFromLSN != 200 || state.Chain.LastToLSN != 300 {
		t.Errorf("state %+v, published %v", state.Chain, env.published())
	}
}

// The old chain is deleted before the new full copy, so a failed full copy
// leaves no chain at all: the next run makes a full copy again
func TestFailedFullCopyAfterRotationIsRetried(t *testing.T) {
	env := newTestEnv(t)
	env.mustBackup()
	env.nextDay()
	env.mustBackup()

	t.Setenv("FAKE_BACKUP_FAIL", "1")
	env.at(2026, 10, 19, 2, 0)
	if err := env.backup(); err == nil {
		t.Fatal("the backup succeeded")
	}
	if got := env.published(); len(got) > 0 {
		t.Errorf("published %v", got)
	}
	if state := env.state(); state.Chain != nil || state.Discarding {
		t.Errorf("state %+v", state)
	}

	t.Setenv("FAKE_BACKUP_FAIL", "")
	env.nextDay()
	env.mustBackup()
	if env.lastCallArg("--incremental-basedir=") != "" {
		t.Error("the retry was not a full copy")
	}
	if state := env.state(); state.Chain == nil || state.Chain.LastType != Full {
		t.Errorf("state %+v", state.Chain)
	}
}

func TestInterruptedRotationIsFinished(t *testing.T) {
	env := newTestEnv(t)
	env.mustBackup()
	env.nextDay()
	env.mustBackup()

	// The run died after deleting the full copy, the incremental one is left
	env.writeState(`{"version": 1, "discarding": true, "chain": null}`)
	if err := os.Remove(filepath.Join(env.app.layout.publish(), firstFull)); err != nil {
		t.Fatal(err)
	}

	env.at(2026, 10, 19, 2, 0)
	env.mustBackup()

	newChain := env.now.UTC().Format(chainIDLayout)
	expectNames(t, "published", env.published(), []string{"mariadb_" + newChain + ".full.xb.zst"})
	expectNames(t, "lsn", env.list(env.app.layout.lsn()), []string{"mariadb_" + newChain + ".full"})
}

func TestLSNDirectoryWithoutItsCopyIsRemoved(t *testing.T) {
	env := newTestEnv(t)
	env.mustBackup()
	orphan := filepath.Join(env.app.layout.lsn(), "mariadb_"+firstChain+".inc-20261012-120000")
	if err := os.Mkdir(orphan, 0750); err != nil {
		t.Fatal(err)
	}

	env.nextDay()
	env.mustBackup()
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the orphan is left: %v", err)
	}
	if got := len(env.published()); got != 2 {
		t.Errorf("got %d copies, want 2", got)
	}
}

func TestForceFullStartsANewChain(t *testing.T) {
	env := newTestEnv(t)
	env.mustBackup()
	env.nextDay()
	env.mustBackup()

	// Even a chain that cannot be continued
	env.writeState("garbage")
	env.nextDay()
	if err := env.app.Backup(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	newChain := env.now.UTC().Format(chainIDLayout)
	expectNames(t, "published", env.published(), []string{"mariadb_" + newChain + ".full.xb.zst"})
	if state := env.state(); state.Chain.ID != newChain {
		t.Errorf("state %+v", state.Chain)
	}
}

// publish/ belongs to the backuper: a stranger file there is neither
// published by mistake nor deleted with the chain
func TestStrangerFileStopsTheRun(t *testing.T) {
	env := newTestEnv(t)
	env.mustBackup()
	stranger := filepath.Join(env.app.layout.publish(), "notes.txt")
	if err := os.WriteFile(stranger, []byte("mine"), 0640); err != nil {
		t.Fatal(err)
	}

	env.at(2026, 10, 19, 2, 0)
	if err := env.backup(); err == nil || !strings.Contains(err.Error(), "notes.txt") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(stranger); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(env.app.layout.publish(), firstFull)); err != nil {
		t.Error(err)
	}
}

func TestInterruptStopsTheBackup(t *testing.T) {
	env := newTestEnv(t)
	t.Setenv("FAKE_SLEEP", "30")

	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(500*time.Millisecond, cancel)
	defer timer.Stop()

	started := time.Now()
	if err := env.app.Backup(ctx, false); err == nil {
		t.Fatal("the backup succeeded")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("the commands were stopped after %v", elapsed)
	}
	if got := env.published(); len(got) > 0 {
		t.Errorf("published %v", got)
	}
	env.expectNothingStaged()
}

func TestCredentialsFileMustBePrivate(t *testing.T) {
	env := newTestEnv(t)
	if err := os.Chmod(env.app.cfg.CredentialsFile, 0644); err != nil {
		t.Fatal(err)
	}
	if err := env.backup(); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("got %v", err)
	}
	if calls := env.callArgs(); len(calls) > 0 {
		t.Error("mariadb-backup was run")
	}
}

func TestOldLogsArePruned(t *testing.T) {
	env := newTestEnv(t)
	env.app.cfg.KeepLogs = 3
	for range 5 {
		env.mustBackup()
		env.nextDay()
	}
	logs := env.list(env.app.layout.logs())
	if len(logs) != 3 {
		t.Fatalf("got logs %v, want 3", logs)
	}
	if !strings.HasSuffix(logs[2], env.now.AddDate(0, 0, -1).UTC().Format(incTagLayout)+logExt) {
		t.Errorf("the newest log is not kept: %v", logs)
	}
}

func TestStatusAndCheck(t *testing.T) {
	env := newTestEnv(t)
	env.mustBackup()
	env.nextDay()
	env.mustBackup()

	var status bytes.Buffer
	if err := env.app.Status(&status); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{firstChain + ", 2 copies", "1 calendar days ago", "renewed on 2026-10-19", "Next run:", "incremental", firstFull} {
		if !strings.Contains(status.String(), want) {
			t.Errorf("status has no %q:\n%s", want, status.String())
		}
	}

	var check bytes.Buffer
	if err := env.app.Check(context.Background(), &check); err != nil {
		t.Fatalf("%v\n%s", err, check.String())
	}
	if strings.Contains(check.String(), "FAIL") {
		t.Errorf("check:\n%s", check.String())
	}

	env.at(2026, 10, 19, 2, 0)
	status.Reset()
	if err := env.app.Status(&status); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.String(), "rotation") {
		t.Errorf("status does not announce the rotation:\n%s", status.String())
	}
}
