package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// The exit codes systemd and the scripts tell the outcomes by
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
	// exitLocked is EX_TEMPFAIL: another run holds the lock, nothing was done
	exitLocked = 75
)

const usage = `Usage: mariadb-backuper <command> [-config path]

Commands:
  setup                 create the MariaDB account, its credentials file and the backup root, then check
  backup [-force-full]  make the daily copy: a full one or an incremental one
  status                show the chain, the last copy and what the next run does
  check                 verify the config, the state and the chain without changing anything

The config is read from ` + defaultConfigPath + ` unless -config is given.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}

	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath, "path to the config file")
	forceFull := false
	switch command {
	case "backup":
		flags.BoolVar(&forceFull, "force-full", false, "delete the current chain and start a new one with a full copy")
	case "setup", "status", "check":
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", command, usage)
		return exitUsage
	}
	if err := flags.Parse(args[1:]); err != nil {
		return exitUsage
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected arguments %v\n\n%s", flags.Args(), usage)
		return exitUsage
	}

	cfg, err := loadConfig(*configPath)
	created := false
	if command == "setup" && errors.Is(err, fs.ErrNotExist) {
		cfg, err = createDefaultConfig(*configPath)
		created = true
	}
	if err != nil {
		fmt.Fprintf(stderr, "config %s: %v\n", *configPath, err)
		return exitUsage
	}
	logger := newLogger(stderr, logLevels[cfg.LogLevel], os.Getenv("JOURNAL_STREAM") != "")
	if created {
		logger.Info("created the config with the defaults", "path", *configPath)
	}
	// The only command that creates the backup root, the others expect it
	if command == "setup" {
		if err := os.MkdirAll(cfg.BackupRoot, dirMode); err != nil {
			logger.Error(err.Error())
			return exitFailed
		}
	}

	app, err := newApp(cfg, logger)
	if err != nil {
		logger.Error(err.Error())
		return exitFailed
	}

	// SIGTERM comes from systemctl stop or from the TimeoutStartSec of the
	// service: the commands are stopped and the copy is not published
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch command {
	case "setup":
		err = app.Setup(ctx, stdout)
	case "backup":
		err = app.Backup(ctx, forceFull)
	case "status":
		err = app.Status(stdout)
	case "check":
		err = app.Check(ctx, stdout)
	}

	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errLocked):
		logger.Error(err.Error())
		return exitLocked
	case errors.Is(err, errInconsistent):
		logger.Error(err.Error())
		logger.Error("the chain is not continued: " + forceFullHint)
		return exitFailed
	default:
		logger.Error(err.Error())
		return exitFailed
	}
}

// newLogger writes to stderr, which systemd sends to the journal. The journal
// stamps the time itself, so it is left out there.
func newLogger(w io.Writer, level slog.Level, journal bool) *slog.Logger {
	options := &slog.HandlerOptions{Level: level}
	if journal {
		options.ReplaceAttr = func(groups []string, attr slog.Attr) slog.Attr {
			if len(groups) == 0 && attr.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return attr
		}
	}
	return slog.New(slog.NewTextHandler(w, options))
}
