package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"
)

var errCheckFailed = errors.New("the check found problems")

// forceFullHint tells what to do with a chain that cannot be continued
const forceFullHint = "inspect the copies and the state, then start a new chain with 'mariadb-backuper backup -force-full' (it deletes the current copies)"

// Status prints the state of the backups. It changes nothing and does not
// wait for a running backup: what it shows may be changing.
func (a *App) Status(w io.Writer) error {
	out := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	row := func(key, format string, args ...any) {
		fmt.Fprintf(out, "%s:\t%s\n", key, fmt.Sprintf(format, args...))
	}

	row("Backup root", "%s", a.layout.root)
	if running, err := lockHeld(a.layout.lock()); err != nil {
		row("Backup running", "unknown: %v", err)
	} else if running {
		row("Backup running", "yes, the state below may be changing")
	} else {
		row("Backup running", "no")
	}
	if free, err := a.freeSpace(a.layout.root); err != nil {
		row("Free space", "unknown: %v", err)
	} else {
		row("Free space", "%s (reserve %d GiB)", formatSize(free), a.cfg.MinFreeSpaceGB)
	}

	r, err := a.reconcile(false)
	if err != nil {
		row("Chain", "BROKEN: %v", err)
		row("Next run", "fails until fixed: %s", forceFullHint)
		return out.Flush()
	}

	now := a.now()
	if r.chain == nil {
		row("Chain", "none")
	} else {
		chain := r.state.Chain
		days := calendarDays(chain.FullStartedAt, now, a.location)
		renewal := chain.FullStartedAt.In(a.location).AddDate(0, 0, a.cfg.FullIntervalDays)
		row("Chain", "%s, %d copies, %s", chain.ID, len(r.chain.Copies), formatSize(uint64(r.chain.Size())))
		row("Full copy", "started %s, %d calendar days ago, renewed on %s",
			a.formatTime(chain.FullStartedAt), days, renewal.Format(time.DateOnly))
		row("Last copy", "%s, %s, finished %s", chain.LastType, chain.LastPath, a.formatTime(chain.LastFinishedAt))
		row("Last LSN", "%d-%d", chain.LastFromLSN, chain.LastToLSN)
	}
	row("Next run", "%s", decide(r.state, now, a.location, a.cfg.FullIntervalDays))
	for _, fix := range r.fixes {
		row("Next run fixes", "%s", fix)
	}
	if err := out.Flush(); err != nil {
		return err
	}

	if r.chain != nil {
		fmt.Fprintln(w)
		table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(table, "COPY\tTYPE\tSIZE\tFROM LSN\tTO LSN")
		for _, item := range r.chain.Copies {
			fmt.Fprintf(table, "%s\t%s\t%s\t%d\t%d\n", item.Name.file(), item.Name.typ,
				formatSize(uint64(item.Size)), item.Checkpoints.FromLSN, item.Checkpoints.ToLSN)
		}
		return table.Flush()
	}
	return nil
}

// Check verifies everything a backup depends on without changing anything
func (a *App) Check(ctx context.Context, w io.Writer) error {
	if running, err := lockHeld(a.layout.lock()); err != nil {
		return err
	} else if running {
		return fmt.Errorf("%w: run the check after it finishes", errLocked)
	}

	problems := 0
	report := func(err error, what string) {
		if err != nil {
			problems++
			fmt.Fprintf(w, "FAIL  %s: %v\n", what, err)
			return
		}
		fmt.Fprintf(w, "OK    %s\n", what)
	}
	warn := func(format string, args ...any) {
		fmt.Fprintf(w, "WARN  %s\n", fmt.Sprintf(format, args...))
	}

	_, err := exec.LookPath(a.cfg.MariadbBackupPath)
	report(err, "mariadb-backup is found")
	_, err = exec.LookPath(a.cfg.ZstdPath)
	report(err, "zstd is found")
	report(checkCredentialsFile(a.cfg.CredentialsFile), "the credentials file is private")
	a.checkDatabase(ctx, report, warn)

	for _, dir := range []string{a.layout.publish(), a.layout.staging(), a.layout.lsn(), a.layout.logs()} {
		if err := requireRealDir(dir); errors.Is(err, os.ErrNotExist) {
			warn("%s does not exist yet, the next backup creates it", dir)
		} else {
			report(err, dir+" is a directory")
		}
	}

	if paths, err := a.leftovers(); err != nil {
		report(err, "leftovers of interrupted runs")
	} else {
		for _, path := range paths {
			warn("%s is a leftover of an interrupted run, the next backup removes it", path)
		}
	}

	r, err := a.reconcile(false)
	if err != nil {
		report(fmt.Errorf("%w; %s", err, forceFullHint), "the state and the chain")
	} else {
		for _, fix := range r.fixes {
			warn("the next backup is going to fix: %s", fix)
		}
		if r.chain == nil {
			report(nil, "the state and the copies agree, there is no chain yet")
		} else {
			report(nil, fmt.Sprintf("the state and the chain %s of %d copies agree, the LSN are continuous", r.chain.ID(), len(r.chain.Copies)))
		}
	}

	if free, err := a.freeSpace(a.layout.root); err != nil {
		report(err, "free space")
	} else {
		var lack error
		if reserve := uint64(a.cfg.MinFreeSpaceGB) * gib; free < reserve {
			lack = fmt.Errorf("%s free, less than the reserve of %s", formatSize(free), formatSize(reserve))
		}
		report(lack, "free space is above the reserve")
	}

	if problems > 0 {
		return fmt.Errorf("%w: %d", errCheckFailed, problems)
	}
	return nil
}

// checkDatabase logs in with the credentials file the way mariadb-backup does
// and checks the privileges of the account and the version of the server
func (a *App) checkDatabase(ctx context.Context, report func(error, string), warn func(string, ...any)) {
	// Who the OS user logs in as through the socket. A user other than root
	// cannot, and then the logins are not compared.
	socketAccount, _ := a.queryRow(ctx, "", "SELECT CURRENT_USER();")
	current, err := a.loginWithCredentials(ctx, socketAccount)
	if err != nil {
		report(fmt.Errorf("%w; run 'mariadb-backuper setup' to set a new password", err), "the credentials file logs in to MariaDB")
		return
	}
	report(nil, "the credentials file logs in to MariaDB as "+current)

	grants, err := a.query(ctx, a.cfg.CredentialsFile, "SHOW GRANTS;")
	if err == nil {
		if missing := missingPrivileges(grants); len(missing) > 0 {
			err = fmt.Errorf("%s missing; run 'mariadb-backuper setup' to grant them", strings.Join(missing, ", "))
		}
	}
	report(err, "the account has "+strings.Join(backupPrivileges, ", "))

	serverVersion, err := a.queryRow(ctx, a.cfg.CredentialsFile, "SELECT VERSION();")
	if err != nil {
		report(err, "the version of the server")
		return
	}
	program, err := exec.LookPath(a.cfg.MariadbBackupPath)
	if err != nil {
		return
	}
	backupVersion, err := mariadbBackupVersion(ctx, program)
	if err != nil {
		report(err, "the version of mariadb-backup")
		return
	}
	same, err := sameVersions(backupVersion, serverVersion)
	switch {
	case err != nil:
		warn("%v", err)
	case same:
		report(nil, "mariadb-backup is of the version of the server, "+serverVersion)
	default:
		warn("mariadb-backup is not of the version of the server (%s, server %s): upgrade the mariadb-backup package", backupVersion, serverVersion)
	}
}

func (a *App) formatTime(t time.Time) string {
	return t.In(a.location).Format("2006-01-02 15:04:05 MST")
}
