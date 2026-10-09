package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func validChainState() ChainState {
	started := time.Date(2026, 10, 11, 23, 0, 4, 0, time.UTC)
	return ChainState{
		ID:             "2026-10-11_23-00",
		FullStartedAt:  started,
		FullPath:       "publish/weekly/mariadb_2026-10-11_23-00.full.xb.zst",
		LastPath:       "publish/weekly/mariadb_2026-10-11_23-00.inc-20261012-230002.xb.zst",
		LastType:       Incremental,
		LastFromLSN:    100,
		LastToLSN:      200,
		LastStartedAt:  started.Add(24 * time.Hour),
		LastFinishedAt: started.Add(25 * time.Hour),
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	chain := validChainState()
	if err := writeState(path, &State{Version: stateVersion, Chain: &chain}); err != nil {
		t.Fatal(err)
	}

	state, err := loadState(path, "mariadb")
	if err != nil {
		t.Fatal(err)
	}
	if *state.Chain != chain {
		t.Errorf("got %+v, want %+v", state.Chain, chain)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != fileMode {
		t.Errorf("mode %v, want %v", info.Mode().Perm(), fileMode)
	}
}

func TestLoadStateRejectsWhatCannotBeTrusted(t *testing.T) {
	cases := map[string]func(state *State){
		"a newer version":              func(state *State) { state.Version = 2 },
		"a chain being deleted":        func(state *State) { state.Discarding = true },
		"an absolute path":             func(state *State) { state.Chain.LastPath = "/etc/passwd" },
		"a path out of publish":        func(state *State) { state.Chain.LastPath = "lsn/mariadb_2026-10-11_23-00.full.xb.zst" },
		"a path with dots":             func(state *State) { state.Chain.FullPath = "publish/weekly/../../state.json" },
		"a full path of another chain": func(state *State) { state.Chain.FullPath = "publish/weekly/mariadb_2026-10-04_23-00.full.xb.zst" },
		"another prefix": func(state *State) {
			state.Chain.LastPath = "publish/weekly/other_2026-10-11_23-00.inc-20261012-230002.xb.zst"
		},
		"a type that does not match": func(state *State) { state.Chain.LastType = Full },
		"an unknown type":            func(state *State) { state.Chain.LastType = "differential" },
		"a full copy with from_lsn": func(state *State) {
			state.Chain.LastPath, state.Chain.LastType = state.Chain.FullPath, Full
		},
		"the LSN backwards":        func(state *State) { state.Chain.LastToLSN = 50 },
		"no full start":            func(state *State) { state.Chain.FullStartedAt = time.Time{} },
		"a start of another chain": func(state *State) { state.Chain.FullStartedAt = state.Chain.FullStartedAt.Add(time.Hour) },
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			chain := validChainState()
			state := &State{Version: stateVersion, Chain: &chain}
			corrupt(state)
			path := filepath.Join(t.TempDir(), "state.json")
			if err := writeState(path, state); err != nil {
				t.Fatal(err)
			}
			if _, err := loadState(path, "mariadb"); !errors.Is(err, errStateCorrupt) {
				t.Errorf("got %v, want errStateCorrupt", err)
			}
		})
	}

	for name, content := range map[string]string{
		"not JSON":       "garbage",
		"cut short":      `{"version": 1, "chain": {"id": `,
		"an unknown key": `{"version": 1, "chain": null, "extra": 1}`,
		"a wrong type":   `{"version": "1", "chain": null}`,
		"two objects":    `{"version": 1, "chain": null} {}`,
		"no version":     `{"chain": null}`,
		"a negative LSN": `{"version": 1, "chain": {"lastToLsn": -1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(content), 0640); err != nil {
				t.Fatal(err)
			}
			if _, err := loadState(path, "mariadb"); !errors.Is(err, errStateCorrupt) {
				t.Errorf("got %v, want errStateCorrupt", err)
			}
		})
	}

	if _, err := loadState(filepath.Join(t.TempDir(), "state.json"), "mariadb"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing state: got %v", err)
	}
}

func TestFailedAtomicWriteKeepsTheOldState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := writeState(path, &State{Version: stateVersion}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	renameFile = func(string, string) error { return errors.New("disk failure") }
	t.Cleanup(func() { renameFile = os.Rename })
	chain := validChainState()
	if err := writeState(path, &State{Version: stateVersion, Chain: &chain}); err == nil {
		t.Fatal("the write succeeded")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("the state changed:\n%s", after)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the temporary file is left: %v", entries)
	}
}
