# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
go test -race ./...                  # тесты; внешние команды — фейковые sh-скрипты из helpers_test.go
bash build.sh                        # статические бинарники linux/amd64 и linux/arm64 → ./build/
bash package.sh 1.0.0                # deb-пакеты из них (nfpm через go run, nfpm.yaml, packaging/)
mariadb-backuper setup|backup|status|check -config /etc/mariadb-backuper.json
```

## Architecture

Один пакет `main`, только стандартная библиотека: `config.go` (JSON-конфиг поверх `defaultConfig`), `layout.go` (раскладка корня, имена копий, `removeInside`), `checkpoints.go` (разбор и проверка `mariadb_backup_checkpoints`), `state.go` (`state.json`, атомарная запись), `chain.go` (скан диска, сверка со state, выбор режима), `backup.go` (ежедневный запуск, конвейер `mariadb-backup | zstd`, публикация, ротация), `status.go` (`status`/`check`), `setup.go` (пользователь БД, credentials-файл, права), `mariadb.go` (клиент `mariadb`, разбор `SHOW GRANTS`, версии), `lock.go` (`flock`), `main.go` (CLI и коды выхода).

Копии забирает качалка (`~/GolandProjects/backuper`, шаблон `nxsBackup`), и формат имён — это контракт с ней: `publish/weekly/<name>_<начало цепочки UTC YYYY-MM-DD_HH-MM>.full|inc-<YYYYMMDD-HHMMSS>.xb.zst`. Дата в имени — начало цепочки у **всех** её копий, иначе качалка удаляла бы инкременты по одному и рвала цепочки; время инкремента — после точки, где качалка его не видит. Времена — UTC, качалка парсит их как UTC. `TestCopyNamesMatchTheDownloader` держит копию её регулярки.

Источник истины — файлы на диске, `state.json` сверяется с ними каждый запуск (`reconcile`). Порядок публикации: проверка в `staging/` → `rename` LSN-каталога в `lsn/` → `rename` архива в `publish/` → запись state. Поэтому `reconcile` чинит только то, что этот порядок может оставить после сбоя: LSN-каталог без архива, state, отстающий ровно на одну копию, незаконченное удаление цепочки (`State.Discarding`), отсутствующий/битый state при однозначно валидной цепочке. Всё остальное — `errInconsistent` и подсказка про `backup -force-full`: продолжать цепочку наугад нельзя, инкремент на неверной базе не восстановится.

Проверка инкремента — `from_lsn` равен `to_lsn` предыдущей копии. На живой 11.4 `to_lsn` — это контрольная точка, и на малонагруженном сервере он может не расти между копиями (`from_lsn == to_lsn` — норма), свежий хвост redo-лога уходит в `last_lsn`.

Ротация считает календарные дни в `App.location` (локальное время сервера), а не прошедшие часы: FULL, начатый поздно, не сдвигает следующий на сутки. Место проверяется до удаления цепочки.

Удаление — только через `removeInside`: путь строго внутри корня, ни один элемент ниже корня не симлинк. Корень резолвится `EvalSymlinks` один раз в `newApp`. В `publish/` и `lsn/` допустимы только файлы с именами копий: чужой файл останавливает запуск, а не удаляется.

Пароль — только в credentials-файле (`--defaults-extra-file` обязан быть первым аргументом и `mariadb-backup`, и `mariadb`); его содержимое не читается, проверяются права (не шире 0600) и владелец. Пользователь и пароль — в группе `[client]`: клиент `mariadb` группу `[mariadb-backup]` игнорирует и без пользователя молча входит как root через сокет, поэтому `loginWithCredentials` отвергает вход, совпавший со входом через сокет. Это нашлось только на живой MariaDB — фейковый `mariadb` в `helpers_test.go` повторяет поведение групп.

Клиентские библиотеки читают `~/.my.cnf` **после** `--defaults-extra-file`, и пароль root оттуда перебивает пароль из credentials-файла (нашлось на продуктивном сервере). Поэтому всё, что входит по credentials-файлу (`mariadb-backup`, проверки), запускается с `credentialsEnv()` — `HOME=/nonexistent`; глобальные `/etc/mysql/*` читаются как обычно. Вход root через сокет окружение не меняет. Фейки в тестах тоже читают `$HOME/.my.cnf`.

`setup` идемпотентен и запускается из `postinst` при каждой установке/обновлении: рабочий credentials-файл не трогается, иначе пароль выделенного пользователя меняется на случайный (`rand.Text()`). SQL с паролем идёт через stdin клиента, ошибки клиента с текстом запроса маскируются; строки `SHOW GRANTS` целиком не логируются — в них хеш пароля.
