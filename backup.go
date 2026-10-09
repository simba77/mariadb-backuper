package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// stopGracePeriod is how long a command has after SIGTERM before it is killed
	stopGracePeriod = time.Minute
	// progressInterval is how often a long backup reports the size written so far
	progressInterval = 15 * time.Minute
	// tailLines is how many last lines of a failed command go to the journal,
	// the whole output is in the log file of the copy
	tailLines = 30
)

// Backup is the daily run: a full copy when there is no chain or the chain is
// due for rotation, an incremental copy on top of the last one otherwise
func (a *App) Backup(ctx context.Context, forceFull bool) error {
	lock, err := acquireLock(a.layout.lock())
	if err != nil {
		return err
	}
	defer func() {
		if err := lock.release(); err != nil {
			a.log.Error("cannot release the lock", "error", err)
		}
	}()

	if err := a.preflight(ctx); err != nil {
		return err
	}
	if err := a.removeLeftovers(); err != nil {
		return err
	}
	defer a.pruneLogs()

	state := &State{Version: stateVersion}
	var chain *Chain
	if forceFull {
		a.log.Warn("a new chain is forced: deleting the current copies")
		size, err := a.publishedSize()
		if err != nil {
			return err
		}
		if err := a.checkSpace(0, size); err != nil {
			return err
		}
		if err := a.discardChain(); err != nil {
			return err
		}
	} else {
		r, err := a.reconcile(true)
		if err != nil {
			return err
		}
		state, chain = r.state, r.chain

		switch decide(state, a.now(), a.location, a.cfg.FullIntervalDays) {
		case modeFull:
			a.log.Info("there is no chain, making a full copy")
			if err := a.checkSpace(0, 0); err != nil {
				return err
			}
		case modeRotate:
			a.log.Info("the full copy is due for renewal, deleting the chain and making a new full copy",
				"chain", chain.ID(), "calendar days", calendarDays(state.Chain.FullStartedAt, a.now(), a.location))
			// Checked before the deletion: the chain is not deleted for a
			// full copy that has no room anyway
			if err := a.checkSpace(uint64(chain.Full().Size), uint64(chain.Size())); err != nil {
				return err
			}
			if err := a.discardChain(); err != nil {
				return err
			}
			state, chain = &State{Version: stateVersion}, nil
		case modeIncremental:
			var expected uint64
			if last := chain.Last(); last.Name.typ == Incremental {
				expected = uint64(last.Size)
			}
			if err := a.checkSpace(expected, 0); err != nil {
				return err
			}
		}
	}

	return a.createCopy(ctx, state, chain)
}

// createCopy makes a copy in the staging directory, verifies it and only then
// publishes it and records it in the state
func (a *App) createCopy(ctx context.Context, state *State, chain *Chain) error {
	started := a.now().UTC()
	var name copyName
	var base *Copy
	if chain == nil {
		name = newFullName(a.cfg.Name, started)
	} else {
		last := chain.Last()
		base = &last
		name = newIncrementalName(a.cfg.Name, chain.ID(), started)
	}

	archive, lsn := a.layout.archive(name), a.layout.lsnOf(name)
	for _, target := range []string{archive, lsn} {
		if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s already exists, a copy with the same name was made a moment ago", target)
		}
	}

	stage := filepath.Join(a.layout.staging(), name.base())
	if err := os.Mkdir(stage, 0700); err != nil {
		return err
	}
	// Whatever happens, the stage is not a copy. A crash leaves it to the
	// next run, which removes it before anything else.
	defer func() {
		if err := removeInside(a.layout.root, stage); err != nil {
			a.log.Error("cannot remove the staging directory", "path", stage, "error", err)
		}
	}()

	logFile, err := os.OpenFile(a.layout.logOf(name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileMode)
	if err != nil {
		return err
	}
	defer func() {
		if err := logFile.Close(); err != nil {
			a.log.Error("cannot close the log file", "path", logFile.Name(), "error", err)
		}
	}()

	part := filepath.Join(stage, name.file()+partSuffix)
	stageLSN := filepath.Join(stage, lsnDir)
	args := a.backupArgs(stageLSN, base)
	a.log.Info("starting the backup", "type", name.typ, "copy", name.file(),
		"command", a.mariadbBackup+" "+strings.Join(args, " "), "log", logFile.Name())

	if err := a.compressBackup(ctx, stage, args, part, logFile); err != nil {
		return fmt.Errorf("the backup failed, the copy is not published: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("interrupted, the copy is not published: %w", err)
	}

	checkpoints, err := a.verifyCopy(ctx, stage, part, stageLSN, name.typ, base, logFile)
	if err != nil {
		return fmt.Errorf("the copy failed verification and is not published: %w", err)
	}

	// The LSN directory goes first: a copy published without it would break
	// the chain, while an LSN directory without its copy is just removed by
	// the next run
	if err := os.Rename(stageLSN, lsn); err != nil {
		return err
	}
	if err := os.Rename(part, archive); err != nil {
		return err
	}
	if err := errors.Join(syncDir(a.layout.lsn()), syncDir(a.layout.publish())); err != nil {
		return err
	}

	info, err := os.Stat(archive)
	if err != nil {
		return err
	}
	published := Copy{Name: name, Size: info.Size(), ModTime: info.ModTime(), Checkpoints: checkpoints}
	finished := a.now()
	next := &State{Version: stateVersion}
	if name.typ == Full {
		next.Chain = ChainState{ID: name.chainID, FullStartedAt: started, FullPath: statePath(name)}.withLast(published, started, finished)
	} else {
		next.Chain = state.Chain.withLast(published, started, finished)
	}
	if err := writeState(a.layout.state(), next); err != nil {
		return fmt.Errorf("the copy %s is published, but the state was not updated (the next run takes the copy into it): %w", name.file(), err)
	}

	a.log.Info("the backup is completed", "type", name.typ, "copy", name.file(), "size", formatSize(uint64(info.Size())),
		"from_lsn", checkpoints.FromLSN, "to_lsn", checkpoints.ToLSN, "duration", finished.Sub(started).Round(time.Second))
	return nil
}

// backupArgs returns the arguments of mariadb-backup. Nothing secret is among
// them: the password comes from the credentials file.
func (a *App) backupArgs(lsnDir string, base *Copy) []string {
	// mariadb-backup accepts the option files only as the first argument
	args := []string{
		"--defaults-extra-file=" + a.cfg.CredentialsFile,
		"--backup",
		"--stream=mbstream",
		// A copy of the checkpoints outside the archive: it is verified
		// before publishing and is the base of the next incremental copy
		"--extra-lsndir=" + lsnDir,
	}
	if base != nil {
		args = append(args, "--incremental-basedir="+a.layout.lsnOf(base.Name))
	}
	if a.cfg.Parallel > 1 {
		args = append(args, "--parallel="+strconv.Itoa(a.cfg.Parallel))
	}
	return args
}

// command runs a program that gets SIGTERM when the run is interrupted, and
// SIGKILL if it has not exited after stopGracePeriod
func (a *App) command(ctx context.Context, dir, program string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = dir
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = stopGracePeriod
	return cmd
}

// compressBackup runs mariadb-backup | zstd > part. Both must succeed: a
// mariadb-backup failure ends the stream early and zstd still exits with 0.
func (a *App) compressBackup(ctx context.Context, dir string, backupArgs []string, part string, log io.Writer) error {
	out, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode)
	if err != nil {
		return err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return errors.Join(err, out.Close())
	}

	backupTail, zstdTail := newTailBuffer(), newTailBuffer()
	backup := a.command(ctx, dir, a.mariadbBackup, backupArgs...)
	backup.Env = credentialsEnv()
	backup.Stdout = writer
	backup.Stderr = io.MultiWriter(log, backupTail)

	compress := a.command(ctx, dir, a.zstd, "-q", "-"+strconv.Itoa(a.cfg.ZstdLevel), "-T"+strconv.Itoa(a.cfg.ZstdThreads), "-c")
	compress.Stdin = reader
	compress.Stdout = out
	compress.Stderr = io.MultiWriter(log, zstdTail)

	if err := compress.Start(); err != nil {
		return errors.Join(fmt.Errorf("start zstd: %w", err), reader.Close(), writer.Close(), out.Close())
	}
	if err := backup.Start(); err != nil {
		// zstd sees the end of its input once the pipe is closed
		closeErr := errors.Join(reader.Close(), writer.Close())
		return errors.Join(fmt.Errorf("start mariadb-backup: %w", err), closeErr, compress.Wait(), out.Close())
	}
	// The commands hold their own ends of the pipe now. Ours are closed, or
	// zstd would never see the end of the stream.
	closeErr := errors.Join(reader.Close(), writer.Close())

	stopProgress := a.reportProgress(part)
	backupErr := backup.Wait()
	compressErr := compress.Wait()
	stopProgress()

	errs := []error{closeErr}
	if backupErr != nil {
		errs = append(errs, a.commandFailed("mariadb-backup", backupErr, backupTail))
	}
	if compressErr != nil {
		errs = append(errs, a.commandFailed("zstd", compressErr, zstdTail))
	}
	// The archive must be on disk before it is published
	errs = append(errs, out.Sync(), out.Close())
	return errors.Join(errs...)
}

// verifyCopy checks the copy in the staging directory: the metadata
// mariadb-backup wrote about it and the integrity of the archive
func (a *App) verifyCopy(ctx context.Context, dir, part, lsnDir string, typ CopyType, base *Copy, log io.Writer) (Checkpoints, error) {
	checkpoints, err := readCheckpoints(lsnDir)
	if err != nil {
		return Checkpoints{}, err
	}
	var baseCheckpoints *Checkpoints
	if base != nil {
		baseCheckpoints = &base.Checkpoints
	}
	if err := checkpoints.verify(typ, baseCheckpoints); err != nil {
		return Checkpoints{}, err
	}

	info, err := os.Stat(part)
	if err != nil {
		return Checkpoints{}, err
	}
	if info.Size() == 0 {
		return Checkpoints{}, errors.New("the archive is empty")
	}

	a.log.Info("testing the archive", "size", formatSize(uint64(info.Size())))
	tail := newTailBuffer()
	test := a.command(ctx, dir, a.zstd, "-q", "-t", part)
	test.Stdout = io.MultiWriter(log, tail)
	test.Stderr = io.MultiWriter(log, tail)
	if err := test.Run(); err != nil {
		return Checkpoints{}, a.commandFailed("zstd -t", err, tail)
	}
	return checkpoints, nil
}

// commandFailed logs the end of the output of a failed command
func (a *App) commandFailed(name string, err error, tail *tailBuffer) error {
	for _, line := range tail.lines(tailLines) {
		a.log.Error(name + ": " + line)
	}
	return fmt.Errorf("%s: %w", name, err)
}

// reportProgress logs the size of the growing archive now and then, so a
// backup running for hours shows in the journal that it is alive
func (a *App) reportProgress(path string) (stop func()) {
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if info, err := os.Stat(path); err != nil {
					a.log.Warn("cannot check the progress", "error", err)
				} else {
					a.log.Info("the backup is in progress", "written", formatSize(uint64(info.Size())))
				}
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// checkSpace is a safeguard, not a forecast: the free space must cover the
// reserve and the size of the previous copy of the same type, when known.
// The space a rotation frees counts too, as it is checked before the deletion.
func (a *App) checkSpace(expected, released uint64) error {
	free, err := a.freeSpace(a.layout.root)
	if err != nil {
		return err
	}
	reserve := uint64(a.cfg.MinFreeSpaceGB) * gib
	a.log.Info("free space", "free", formatSize(free), "released by the rotation", formatSize(released),
		"expected copy size", formatSize(expected), "reserve", formatSize(reserve))

	if free+released < reserve+expected {
		return fmt.Errorf("not enough free space in %s: %s free and %s to be released, need the reserve of %s (minFreeSpaceGB) and %s for the copy (the size of the previous one)",
			a.layout.root, formatSize(free), formatSize(released), formatSize(reserve), formatSize(expected))
	}
	return nil
}

// discardChain deletes every copy and LSN directory. The state is marked
// first, so a run interrupted halfway is finished by the next one instead of
// leaving a chain with some of its copies missing.
func (a *App) discardChain() error {
	if err := writeState(a.layout.state(), &State{Version: stateVersion, Discarding: true}); err != nil {
		return fmt.Errorf("mark the chain as being deleted: %w", err)
	}

	archives, lsnDirs, err := a.entries()
	if err != nil {
		return err
	}
	for _, archive := range archives {
		if err := removeInside(a.layout.root, a.layout.archive(archive.name)); err != nil {
			return err
		}
		a.log.Info("deleted the copy", "copy", archive.name.file(), "size", formatSize(uint64(archive.info.Size())))
	}
	for _, name := range lsnDirs {
		if err := removeInside(a.layout.root, a.layout.lsnOf(name)); err != nil {
			return err
		}
	}
	if err := errors.Join(syncDir(a.layout.publish()), syncDir(a.layout.lsn())); err != nil {
		return err
	}

	return writeState(a.layout.state(), &State{Version: stateVersion})
}

// publishedSize is the size of every published copy, the space a new chain frees
func (a *App) publishedSize() (uint64, error) {
	archives, _, err := a.entries()
	if err != nil {
		return 0, err
	}
	var size uint64
	for _, archive := range archives {
		size += uint64(archive.info.Size())
	}
	return size, nil
}

// leftovers lists what interrupted runs left behind: the staging
// directories and the temporary files of the state. None of them is a copy.
func (a *App) leftovers() ([]string, error) {
	entries, err := readDirIfExists(a.layout.staging())
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		paths = append(paths, filepath.Join(a.layout.staging(), entry.Name()))
	}

	temps, err := filepath.Glob(filepath.Join(a.layout.root, "."+stateFileName+".*.tmp"))
	if err != nil {
		return nil, err
	}
	return append(paths, temps...), nil
}

// removeLeftovers removes the leftovers of interrupted runs. It runs under the
// lock, so nothing is writing them anymore.
func (a *App) removeLeftovers() error {
	paths, err := a.leftovers()
	if err != nil {
		return err
	}
	for _, path := range paths {
		a.log.Warn("removing a leftover of an interrupted run: it was never verified and is not a copy", "path", path)
		if err := removeInside(a.layout.root, path); err != nil {
			return err
		}
	}
	return nil
}

// pruneLogs keeps the logs of the last keepLogs runs. Their names sort in the
// order the runs were made.
func (a *App) pruneLogs() {
	entries, err := readDirIfExists(a.layout.logs())
	if err != nil {
		a.log.Error("cannot list the logs", "error", err)
		return
	}

	var logs []string
	for _, entry := range entries {
		base, ok := strings.CutSuffix(entry.Name(), logExt)
		if _, parsed := parseCopyBase(base); ok && parsed && entry.Type().IsRegular() {
			logs = append(logs, entry.Name())
		}
	}
	sort.Strings(logs)

	for len(logs) > a.cfg.KeepLogs {
		path := filepath.Join(a.layout.logs(), logs[0])
		logs = logs[1:]
		if err := removeInside(a.layout.root, path); err != nil {
			a.log.Error("cannot delete an old log", "path", path, "error", err)
			continue
		}
		a.log.Debug("deleted an old log", "path", path)
	}
}
