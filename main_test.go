package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunExitCodes(t *testing.T) {
	env := newTestEnv(t)
	config := writeConfig(t, env.app.cfg)

	cases := []struct {
		args []string
		want int
	}{
		{nil, exitUsage},
		{[]string{"restore"}, exitUsage},
		{[]string{"backup", "-config", filepath.Join(t.TempDir(), "missing.json")}, exitUsage},
		{[]string{"status", "-config", config, "extra"}, exitUsage},
		{[]string{"check", "-config", config}, exitOK},
		{[]string{"status", "-config", config}, exitOK},
	}
	for _, c := range cases {
		var stdout, stderr bytes.Buffer
		if got := run(c.args, &stdout, &stderr); got != c.want {
			t.Errorf("%v: exit code %d, want %d\n%s%s", c.args, got, c.want, stdout.String(), stderr.String())
		}
	}
}

func TestRunReportsABrokenChain(t *testing.T) {
	env := newTestEnv(t)
	env.mustBackup()
	env.writeState(`{"version": 1, "chain": null}`)

	var stdout, stderr bytes.Buffer
	if got := run([]string{"backup", "-config", writeConfig(t, env.app.cfg)}, &stdout, &stderr); got != exitFailed {
		t.Errorf("exit code %d, want %d", got, exitFailed)
	}
	if !strings.Contains(stderr.String(), "-force-full") {
		t.Errorf("no advice in:\n%s", stderr.String())
	}
}
