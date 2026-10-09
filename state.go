package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const stateVersion = 1

// State is what state.json records. The copies on disk are checked against it
// on every run: a state that does not match them stops the backups instead of
// continuing the chain blindly.
type State struct {
	Version int `json:"version"`
	// Discarding is set while the chain is being deleted for a new full copy,
	// so a run interrupted halfway is finished by the next one
	Discarding bool `json:"discarding,omitempty"`
	// Chain is nil when there is no chain: before the first full copy and
	// after a rotation until the new full copy succeeds
	Chain *ChainState `json:"chain"`
}

type ChainState struct {
	ID string `json:"id"`
	// FullStartedAt is when the full copy was started. The rotation counts the
	// calendar days from it.
	FullStartedAt time.Time `json:"fullStartedAt"`
	// The paths are relative to the backup root
	FullPath       string    `json:"fullPath"`
	LastPath       string    `json:"lastPath"`
	LastType       CopyType  `json:"lastType"`
	LastFromLSN    uint64    `json:"lastFromLsn"`
	LastToLSN      uint64    `json:"lastToLsn"`
	LastStartedAt  time.Time `json:"lastStartedAt"`
	LastFinishedAt time.Time `json:"lastFinishedAt"`
}

var errStateCorrupt = errors.New("the state file is corrupt")

// loadState reads and validates the state. A missing file is reported as
// fs.ErrNotExist, a file that cannot be trusted as errStateCorrupt.
func loadState(path, prefix string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var state State
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return nil, fmt.Errorf("%w: %v", errStateCorrupt, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: unexpected data after the state object", errStateCorrupt)
	}
	if err := state.validate(prefix); err != nil {
		return nil, fmt.Errorf("%w: %v", errStateCorrupt, err)
	}
	return &state, nil
}

func (s *State) validate(prefix string) error {
	if s.Version != stateVersion {
		return fmt.Errorf("unsupported version %d, expected %d", s.Version, stateVersion)
	}
	if s.Discarding && s.Chain != nil {
		return errors.New("a chain being deleted cannot be the current one")
	}
	if s.Chain == nil {
		return nil
	}
	return s.Chain.validate(prefix)
}

func (c *ChainState) validate(prefix string) error {
	var errs []error

	full, err := parseStatePath(c.FullPath)
	if err != nil {
		errs = append(errs, fmt.Errorf("fullPath: %w", err))
	} else if full.typ != Full || full.prefix != prefix || full.chainID != c.ID {
		errs = append(errs, fmt.Errorf("fullPath %q is not the full copy of the chain %q", c.FullPath, c.ID))
	}

	last, err := parseStatePath(c.LastPath)
	if err != nil {
		errs = append(errs, fmt.Errorf("lastPath: %w", err))
	} else if last.prefix != prefix || last.chainID != c.ID {
		errs = append(errs, fmt.Errorf("lastPath %q is not a copy of the chain %q", c.LastPath, c.ID))
	} else if last.typ != c.LastType {
		errs = append(errs, fmt.Errorf("lastPath %q is not of lastType %q", c.LastPath, c.LastType))
	}

	switch c.LastType {
	case Full:
		if c.LastPath != c.FullPath {
			errs = append(errs, errors.New("the last copy is full, but it is not the full copy of the chain"))
		}
		if c.LastFromLSN != 0 {
			errs = append(errs, fmt.Errorf("lastFromLsn of a full copy must be 0, got %d", c.LastFromLSN))
		}
	case Incremental:
	default:
		errs = append(errs, fmt.Errorf("unknown lastType %q", c.LastType))
	}
	if c.LastToLSN < c.LastFromLSN {
		errs = append(errs, fmt.Errorf("lastToLsn %d is less than lastFromLsn %d", c.LastToLSN, c.LastFromLSN))
	}

	if c.FullStartedAt.IsZero() || c.LastStartedAt.IsZero() || c.LastFinishedAt.IsZero() {
		errs = append(errs, errors.New("fullStartedAt, lastStartedAt and lastFinishedAt are required"))
	} else if c.FullStartedAt.UTC().Format(chainIDLayout) != c.ID {
		errs = append(errs, fmt.Errorf("fullStartedAt %s does not match the chain %q", c.FullStartedAt.Format(time.RFC3339), c.ID))
	}

	return errors.Join(errs...)
}

// withLast returns the chain state with the given copy as the last one
func (c ChainState) withLast(last Copy, started, finished time.Time) *ChainState {
	c.LastPath = statePath(last.Name)
	c.LastType = last.Name.typ
	c.LastFromLSN = last.Checkpoints.FromLSN
	c.LastToLSN = last.Checkpoints.ToLSN
	c.LastStartedAt = started.UTC()
	c.LastFinishedAt = finished.UTC()
	return &c
}

func writeState(path string, state *State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), fileMode)
}

// renameFile is os.Rename, replaced in the tests to make the atomic write fail
var renameFile = os.Rename

// writeFileAtomic replaces the file through a temporary one in the same
// directory, so a crash leaves either the old content or the new one
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}

	err = func() error {
		if _, err := temp.Write(data); err != nil {
			return err
		}
		if err := temp.Chmod(perm); err != nil {
			return err
		}
		return temp.Sync()
	}()
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = renameFile(temp.Name(), path)
	}
	if err != nil {
		if removeErr := os.Remove(temp.Name()); removeErr != nil {
			return errors.Join(err, fmt.Errorf("remove the temporary file: %w", removeErr))
		}
		return err
	}

	return syncDir(dir)
}
