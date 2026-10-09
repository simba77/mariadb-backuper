# MariaDB Backuper

Makes physical backups of a MariaDB server with `mariadb-backup`: one full copy a week and an incremental copy
every day on top of it. Only the current chain is kept on the server. The copies are published in the layout the
[backup downloader](https://github.com/simba77/backup-downloader) understands, so it can take them off the server
and keep as many as its disk allows.

Built and checked against MariaDB 11.4 on Ubuntu 24.04.

## How it works

`mariadb-backuper backup` runs once a day from a systemd timer:

1. Takes an exclusive `flock`. Another run in progress makes it exit with code 75 and do nothing.
2. Checks `mariadb-backup`, `zstd`, the credentials file and the backup root, then removes the leftovers of runs
   that were interrupted (they were never verified and are never taken for copies).
3. Checks the state against the copies on disk: names, LSN directories and the LSN sequence. A state that does not
   match the disk stops the run, see [When something goes wrong](#when-something-goes-wrong).
4. Picks the mode:
   * no valid full copy — a **full** copy;
   * `fullIntervalDays` (7) **calendar days** have passed since the day of the full copy — a **rotation**: the
     whole chain is deleted and a new full copy is made. The time of the day is ignored, so a full copy started
     late does not push the next one by a day. The days are counted in the local time of the server;
   * otherwise an **incremental** copy on top of the last copy of the chain.
5. Streams the copy into the staging directory:
   `mariadb-backup --backup --stream=mbstream --extra-lsndir=… | zstd`.
6. Verifies it: the exit codes of both commands, the checkpoints (`backup_type`, `from_lsn` continuing `to_lsn` of
   the previous copy) and the integrity of the archive with `zstd -t`.
7. Only then moves it to `publish/` and writes the state atomically.

A failed run leaves the state and the published copies as they were. The full output of `mariadb-backup` goes to
the log file of the copy, the journal gets the summary and the last lines of a failed command.

### Layout

```text
/var/backups/mariadb/
├── publish/weekly/   ← backupsPath of the downloader: verified copies only
│   ├── mariadb_2026-10-11_23-00.full.xb.zst
│   ├── mariadb_2026-10-11_23-00.inc-20261012-230002.xb.zst
│   └── ...
├── staging/          ← a copy being made, never seen by the downloader
├── lsn/<copy>/       ← mariadb_backup_checkpoints of every copy, the base of the next incremental one
├── logs/<copy>.log   ← the output of mariadb-backup, the last keepLogs runs
├── state.json
└── lock
```

Every copy is a single archive: a `mbstream` stream compressed with `zstd`. Everything needed to restore it — the
data, the redo log and the checkpoints — is inside. The `lsn/` directories are only for the backuper itself.

### File names

The names follow the `nxsBackup` template of the downloader, `<period>/<name>_YYYY-MM-DD_HH-MM.<ext>`, with the
date being **the start of the chain** for every copy of it. The downloader takes the age of a file from that date,
so all the copies of a chain expire on its side together: an incremental copy is useless without the full copy and
every incremental copy before it. The start of an incremental copy goes after the dot, where the downloader does
not look. All the times in the names are **UTC**, the way the downloader parses them: a copy made at 02:00 in
Moscow is named `23-00` of the day before.

The names sort in the order the copies were made, which is the order to restore them in.

### Accepted risk

A rotation deletes the old chain **before** the new full copy is made: the server has room for one chain only.
If the new full copy fails, the server has no local copy until a later run succeeds — every next run retries the
full copy. The copies already taken by the downloader are not affected. This risk is accepted on purpose and the
backuper does not try to work around it.

The backuper and the downloader know nothing about each other: no acknowledgements, no locks between the machines.

## Installation

### From the package

Download the package for the architecture of the server from a release and install it with apt, which brings
`mariadb-backup`, `zstd` and the `mariadb` client along:

```shell
apt install ./mariadb-backuper_1.0.0_amd64.deb
```

The package puts the binary to `/usr/bin`, the units to `/usr/lib/systemd/system` and the config to
`/etc/mariadb-backuper.json`, then:

1. runs `mariadb-backuper setup`, see [Setup](#setup);
2. enables and starts the timer on the first installation.

A setup that fails, for example while MariaDB is stopped, does not fail the installation: the package is installed,
the error is printed, and `sudo mariadb-backuper setup` is to be run once the reason is fixed. `mariadb-backuper
check` then fails until it is.

An upgrade is the same `apt install` of a newer package. It keeps the edited config, runs the setup again, which
changes nothing that works, and leaves the timer enabled or disabled the way it was.

`apt remove` stops and disables the timer. `apt purge` also deletes the config and the credentials file written by
the setup. The copies in `/var/backups/mariadb` and the MariaDB account are never deleted by the package:

```shell
rm -rf /var/backups/mariadb
mariadb -e "DROP USER 'mariadb-backup'@'localhost'"
```

### Without the package

```shell
apt install mariadb-backup zstd
install -m 755 build/linux-amd64 /usr/bin/mariadb-backuper
mariadb-backuper setup        # also writes the default config when there is none
install -m 644 systemd/mariadb-backuper.service systemd/mariadb-backuper.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now mariadb-backuper.timer
```

### Setup

`mariadb-backuper setup` prepares the server and then runs the [check](#commands). It has to run as root while
MariaDB is running: it connects with the `mariadb` client as root through the socket, the way root logs in on
Ubuntu without a password. Every step looks first and changes only what is missing or broken:

* the backup root and its directories are created with mode `0750`;
* a credentials file that logs in is kept as it is; otherwise the account `'mariadb-backup'@'localhost'` gets a new
  random password — it is created when there is none — and the file is written anew with mode `0600`;
* the account gets the privileges below; anything it has beyond them is pointed out in the log;
* the check compares the version of `mariadb-backup` with the version of the server.

The password never appears in the config, the arguments of the processes or the logs: the statements reach the
client through its stdin, and `mariadb-backup` reads the password from the file passed with `--defaults-extra-file`.

The privileges `mariadb-backup` needs according to the MariaDB documentation:

| Privilege | What for |
|---|---|
| `RELOAD` | Flushing and the backup locks (`BACKUP STAGE`) |
| `PROCESS` | Reading the state of the server and InnoDB |
| `LOCK TABLES` | The short lock at the end of the copy that makes it consistent |
| `BINLOG MONITOR` | Recording the binary log position in the copy |

Not granted, as nothing here needs them: `SUPER` (MyRocks only), `CONNECTION ADMIN` (`--kill-long-queries-timeout`
only), `REPLICA MONITOR` (`--slave-info`, `--galera-info` only), grants on `mysql.mariadb_backup_history`
(`--history` only).

#### Credentials file

The setup writes it this way:

```ini
# Written by mariadb-backuper setup: the account of the backups
[client]
user=mariadb-backup
password=<random>
```

A file made by hand must keep the user and the password in the `[client]` group too: `mariadb-backup` reads it, and
so does the `mariadb` client of the checks, while the client ignores `[mariadb-backup]`. Without a user it would
silently log in as root through the socket; the setup and the check refuse such a file. The backuper also refuses
to run when the file is open to the group or other users, or is owned by someone else.

`mariadb-backup` and the checks run with `HOME=/nonexistent`: the client libraries read `~/.my.cnf` after the
`--defaults-extra-file`, and a password of root kept there would override the one of the backup account. The
global option files in `/etc/mysql` are read as usual.

### Config

The file is JSON. A key left out keeps its default, an unknown key is an error.

| Field | Default | Description |
|---|---|---|
| `backupRoot` | `/var/backups/mariadb` | The root of the layout above, created by the setup |
| `name` | `mariadb` | The prefix of the file names: lowercase letters, digits and dashes |
| `credentialsFile` | `/etc/mysql/mariadb-backup.cnf` | The option file with the user and the password |
| `mariadbBackupPath` | `mariadb-backup` | Looked up in `PATH` unless it is a path |
| `zstdPath` | `zstd` | Looked up in `PATH` unless it is a path |
| `mariadbPath` | `mariadb` | The client of the setup and the check, looked up in `PATH` unless it is a path |
| `fullIntervalDays` | `7` | Calendar days a full copy starts a chain for |
| `minFreeSpaceGB` | `20` | The free space that must be left on top of the expected copy size |
| `zstdLevel` | `3` | 1–19. Higher levels compress better and take much more CPU |
| `zstdThreads` | `2` | Compression threads, `0` takes every core |
| `parallel` | `1` | Threads `mariadb-backup` copies the data files with (`--parallel`) |
| `keepLogs` | `30` | How many logs of the runs to keep in `logs/` |
| `logLevel` | `info` | `debug`, `info`, `warn` or `error` |

There is no timeout of its own: a run is limited by `TimeoutStartSec` of the service.

#### Free space

The check before a copy is a safeguard, not a forecast: the free space must cover `minFreeSpaceGB` and the size
of the previous copy of the same type, when there is one. A rotation counts the space the old chain frees, and
checks it **before** deleting the chain. The chain is never deleted for lack of space outside the rotation.

The size of a compressed copy depends on the data: measure it on the first full copy and the first incremental
ones (`mariadb-backuper status`) and set `minFreeSpaceGB` from that.

### systemd

```shell
systemctl list-timers mariadb-backuper.timer   # when the next run is
systemctl status mariadb-backuper.service      # how the last one went
```

The service:

* runs as `root` with `UMask=0027`: `mariadb-backup` reads the datadir, the copies are not readable by others;
* is `Type=oneshot`, so the timer never starts a second run while one is going, and the lock covers the manual runs;
* allows a run of up to 20 hours (`TimeoutStartSec`), and on a stop gives `mariadb-backup` a minute to exit;
* runs with a lower CPU and IO priority (`Nice=10`, `IOSchedulingPriority=7`), so the database comes first;
* has a moderate hardening (`ProtectSystem=full`, `PrivateTmp`, …) that leaves `/var` writable. If
  `mariadb-backup` fails with "Permission denied" or "Read-only file system", check the journal and relax the
  options of the unit with `systemctl edit mariadb-backuper.service`.

The timer runs at 02:00 of the local time. With `Persistent=true` a run missed while the server was off is made
right after the boot, which may be in the working hours.

To make a copy right away (a real backup of the database):

```shell
systemctl start mariadb-backuper.service
```

## Commands

```shell
mariadb-backuper setup               # the account, the credentials file and the backup root, then the check
mariadb-backuper backup              # the daily run, what the timer starts
mariadb-backuper backup -force-full  # delete the current chain and start a new one
mariadb-backuper status              # the chain, the copies, the LSN, the free space and what the next run does
mariadb-backuper check               # verify everything, the login to MariaDB included, without changing anything
```

Every command takes `-config <path>`, the default is `/etc/mariadb-backuper.json`.

| Exit code | Meaning |
|---|---|
| `0` | Done. For `check` — no problems |
| `1` | The run failed, see the log. For `check` — problems found |
| `2` | A wrong command line or config |
| `75` | Another run is in progress, nothing was done |

## Logs

```shell
journalctl -u mariadb-backuper              # every run
journalctl -u mariadb-backuper -b -p err    # the errors since the boot
ls /var/backups/mariadb/logs/               # the full output of mariadb-backup, a file per run
```

A long copy reports the size written so far every 15 minutes.

## The downloader

Set up the server in the [downloader](https://github.com/simba77/backup-downloader) config:

```json
{
  "active": true,
  "name": "mariadb",
  "backupsPath": "/var/backups/mariadb/publish/",
  "pathTemplate": "nxsBackup",
  "daysCount": 14,
  "minCopies": 7,
  "maxParallelDownloads": 1,
  "type": "sftp",
  "server": "203.0.113.10",
  "user": "root",
  "password": "",
  "port": 22
}
```

* `daysCount: 14` keeps exactly **two chains**: the previous week and the current one. The depth of the history
  goes from 7 days right after a new full copy to 13 days before the next one. For at least 14 days at all times
  set `daysCount: 22`, which keeps up to three full copies.
* `minCopies: 7` (a full copy and up to six incremental ones) keeps the whole last chain when the backups stop
  being made: the downloader then does not delete its last copies by age.
* A copy is published only when it is complete, so a full copy still being made at the time of the download is
  taken the next day.
* If the downloader logs in as a user other than `root`, give its group read access to `publish/`.

## Restore

A restore needs `mariadb-backup` of the same version as the server, `zstd`, and free space for the unpacked
copies. Take the full copy of the chain and the incremental copies **in the order of their names**, up to the
day you need.

1. Unpack every copy into its own directory:

   ```shell
   mkdir -p /restore/0-full /restore/1-inc /restore/2-inc
   cd /restore/0-full && zstd -dc /backups/mariadb_2026-10-11_23-00.full.xb.zst | mbstream -x
   cd /restore/1-inc  && zstd -dc /backups/mariadb_2026-10-11_23-00.inc-20261012-230002.xb.zst | mbstream -x
   cd /restore/2-inc  && zstd -dc /backups/mariadb_2026-10-11_23-00.inc-20261013-230004.xb.zst | mbstream -x
   ```

2. Prepare the full copy, then apply the incremental ones to it one by one, in order:

   ```shell
   mariadb-backup --prepare --target-dir=/restore/0-full
   mariadb-backup --prepare --target-dir=/restore/0-full --incremental-dir=/restore/1-inc
   mariadb-backup --prepare --target-dir=/restore/0-full --incremental-dir=/restore/2-inc
   ```

   An incremental copy applied out of order is refused with "This incremental backup seems not to be proper for
   the target". Each step must end with "completed OK!".

3. Put the data in place of an **empty** datadir and start the server:

   ```shell
   systemctl stop mariadb
   mv /var/lib/mysql /var/lib/mysql.old && mkdir /var/lib/mysql
   mariadb-backup --copy-back --target-dir=/restore/0-full
   chown -R mysql:mysql /var/lib/mysql
   systemctl start mariadb
   ```

The procedure was checked on MariaDB 11.4.13: a chain of a full and four incremental copies restored to the same
rows and checksums as the source.

## When something goes wrong

| Situation | What to do |
|---|---|
| A run failed | `journalctl -u mariadb-backuper` and `logs/<copy>.log`. The state is not changed, the next run tries again |
| "Access denied" for `mariadb-backup` | The password or the privileges changed: `mariadb-backuper setup` sets them anew |
| "another mariadb-backuper run is in progress" | Wait for it: `systemctl status mariadb-backuper` |
| "not enough free space" | Free the space or lower `minFreeSpaceGB`. Nothing was deleted |
| "the state and the copies on disk contradict each other" | Run `mariadb-backuper check` and look at what it reports. The chain is not continued: incremental copies made on top of a wrong base cannot be restored. When the reason is understood, start a new chain with `mariadb-backuper backup -force-full` |
| "unexpected … in publish" | A stranger file in `publish/weekly/` or `lsn/`. Move it out: these directories belong to the backuper |
| A new full copy failed after a rotation | Nothing to do: the next run makes a full copy again. Until then the server has no local copy, see [Accepted risk](#accepted-risk) |

Fixed by the next run by itself, with a warning in the journal:

* the staging directory of an interrupted run — removed;
* a copy published right before a crash, before the state was written — verified already, taken into the state;
* an LSN directory without its copy — removed;
* a rotation interrupted while deleting the chain — finished;
* a missing or corrupt `state.json` while the copies form one valid chain — rebuilt from the copies, the corrupt
  file is kept as `state.json.corrupt-<time>`.

## Development

Requires Go 1.26 or newer, as set in `go.mod`. No dependencies outside the standard library.

```shell
go test -race ./...     # the external commands are fake shell scripts, no MariaDB needed
bash build.sh           # static Linux binaries in build/
bash package.sh 1.0.0   # deb packages of them in build/, nfpm runs through go run
```

The package is described in [nfpm.yaml](nfpm.yaml), its maintainer scripts are in [packaging/](packaging/).

### Releases

Pushing a tag that starts with `v` runs [.github/workflows/release.yml](.github/workflows/release.yml): it vets,
tests, builds the platforms of `build.sh`, packages them and attaches the binaries and the deb packages to a GitHub
release named after the tag.

```shell
git tag -a v1.0.0 -m v1.0.0 && git push origin v1.0.0
```
