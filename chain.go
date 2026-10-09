package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// errInconsistent stops a run when the state and the copies on disk
// contradict each other: continuing the chain could produce copies that
// cannot be restored
var errInconsistent = errors.New("the state and the copies on disk contradict each other")

// Copy is a published copy with the metadata from its LSN directory
type Copy struct {
	Name        copyName
	Size        int64
	ModTime     time.Time
	Checkpoints Checkpoints
}

// Chain is a full copy followed by the incremental copies made on top of it
type Chain struct {
	Copies []Copy
}

func (c *Chain) ID() string { return c.Copies[0].Name.chainID }
func (c *Chain) Full() Copy { return c.Copies[0] }
func (c *Chain) Last() Copy { return c.Copies[len(c.Copies)-1] }

func (c *Chain) Size() int64 {
	var size int64
	for _, item := range c.Copies {
		size += item.Size
	}
	return size
}

// archiveEntry is a file in publish/
type archiveEntry struct {
	name copyName
	info fs.FileInfo
}

// entries lists the archives in publish/ and the directories in lsn/. Anything
// else there stops the run: both directories belong to the backuper alone, and
// a stranger file would be downloaded or deleted by mistake.
func (a *App) entries() ([]archiveEntry, []copyName, error) {
	files, err := readDirIfExists(a.layout.publish())
	if err != nil {
		return nil, nil, err
	}
	var archives []archiveEntry
	for _, file := range files {
		info, err := file.Info()
		if err != nil {
			return nil, nil, err
		}
		name, ok := parseCopyFile(file.Name())
		if !ok || name.prefix != a.cfg.Name || !info.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("unexpected %s in %s: only the copies of this backuper may be there", file.Name(), a.layout.publish())
		}
		archives = append(archives, archiveEntry{name: name, info: info})
	}

	dirs, err := readDirIfExists(a.layout.lsn())
	if err != nil {
		return nil, nil, err
	}
	var lsnDirs []copyName
	for _, dir := range dirs {
		name, ok := parseCopyBase(dir.Name())
		if !ok || name.prefix != a.cfg.Name || !dir.IsDir() {
			return nil, nil, fmt.Errorf("unexpected %s in %s: only the LSN directories of the copies may be there", dir.Name(), a.layout.lsn())
		}
		lsnDirs = append(lsnDirs, name)
	}

	return archives, lsnDirs, nil
}

// scan reads the published copies, sorted from the oldest, and the LSN
// directories left without their copy by a run interrupted while publishing
func (a *App) scan() ([]Copy, []copyName, error) {
	archives, lsnDirs, err := a.entries()
	if err != nil {
		return nil, nil, err
	}

	// os.ReadDir sorts by name, which is the chronological order of the copies
	copies := make([]Copy, 0, len(archives))
	published := make(map[string]bool)
	for _, archive := range archives {
		checkpoints, err := readCheckpoints(a.layout.lsnOf(archive.name))
		if err != nil {
			return nil, nil, fmt.Errorf("%w: copy %s: %v", errInconsistent, archive.name.file(), err)
		}
		copies = append(copies, Copy{
			Name:        archive.name,
			Size:        archive.info.Size(),
			ModTime:     archive.info.ModTime(),
			Checkpoints: checkpoints,
		})
		published[archive.name.base()] = true
	}

	var orphans []copyName
	for _, name := range lsnDirs {
		if !published[name.base()] {
			orphans = append(orphans, name)
		}
	}
	return copies, orphans, nil
}

// buildChain checks that the copies form one chain: a full copy and the
// incremental copies each continuing the LSN of the previous one
func buildChain(copies []Copy) (*Chain, error) {
	if len(copies) == 0 {
		return nil, nil
	}

	full := copies[0]
	if full.Name.typ != Full {
		return nil, fmt.Errorf("the chain has no full copy, the oldest copy is %s", full.Name.file())
	}
	for i, item := range copies {
		if item.Name.chainID != full.Name.chainID {
			return nil, fmt.Errorf("copies of different chains: %s and %s", full.Name.file(), item.Name.file())
		}
		var base *Checkpoints
		if i > 0 {
			if item.Name.typ != Incremental {
				return nil, fmt.Errorf("a second full copy %s in the chain", item.Name.file())
			}
			base = &copies[i-1].Checkpoints
		}
		if err := item.Checkpoints.verify(item.Name.typ, base); err != nil {
			return nil, fmt.Errorf("copy %s: %w", item.Name.file(), err)
		}
	}
	return &Chain{Copies: copies}, nil
}

// reconciliation is the state and the chain that agree with each other
type reconciliation struct {
	state *State
	chain *Chain
	// fixes are what was done, or would be done, to make them agree
	fixes []string
}

// reconcile checks the state against the copies on disk. With apply it also
// fixes what can be fixed without guessing and logs it; without it, it only
// reports the fixes, for the check and status commands.
//
// What is fixed: a rotation interrupted while deleting the chain, the LSN
// directory of a copy that was never published, a state lagging behind by the
// last copy (it is published before the state is written), and a missing or
// corrupt state when the copies form one valid chain. Any other disagreement
// is an error: the chain is not continued until someone looks at it.
func (a *App) reconcile(apply bool) (*reconciliation, error) {
	r := &reconciliation{}
	fix := func(description string, action func() error) error {
		r.fixes = append(r.fixes, description)
		if !apply {
			return nil
		}
		a.log.Warn(description)
		return action()
	}

	state, stateErr := loadState(a.layout.state(), a.cfg.Name)
	if stateErr != nil && !errors.Is(stateErr, fs.ErrNotExist) && !errors.Is(stateErr, errStateCorrupt) {
		return nil, stateErr
	}

	if stateErr == nil && state.Discarding {
		if err := fix("finishing the deletion of the chain interrupted during a rotation", a.discardChain); err != nil {
			return nil, err
		}
		r.state = &State{Version: stateVersion}
		return r, nil
	}

	copies, orphans, err := a.scan()
	if err != nil {
		return nil, err
	}
	for _, orphan := range orphans {
		description := fmt.Sprintf("removing the LSN directory %s left without its copy by an interrupted run", orphan.base())
		if err := fix(description, func() error { return removeInside(a.layout.root, a.layout.lsnOf(orphan)) }); err != nil {
			return nil, err
		}
	}

	chain, chainErr := buildChain(copies)
	switch {
	case stateErr == nil && state.Chain == nil:
		if len(copies) > 0 {
			return nil, fmt.Errorf("%w: the state records no chain, but there are copies in %s", errInconsistent, a.layout.publish())
		}
		r.state = state

	case stateErr == nil:
		if chainErr != nil {
			return nil, fmt.Errorf("%w: %v", errInconsistent, chainErr)
		}
		if chain == nil {
			return nil, fmt.Errorf("%w: the state records the chain %s, but there are no copies in %s", errInconsistent, state.Chain.ID, a.layout.publish())
		}
		lagging, err := matchState(state.Chain, chain)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errInconsistent, err)
		}
		r.state = state
		if lagging {
			last := chain.Last()
			r.state = &State{Version: stateVersion, Chain: state.Chain.withLast(last, last.Name.started, last.ModTime)}
			description := fmt.Sprintf("taking the copy %s into the state: it was published and verified, but the run stopped before recording it", last.Name.file())
			if err := fix(description, func() error { return writeState(a.layout.state(), r.state) }); err != nil {
				return nil, err
			}
		}

	case errors.Is(stateErr, fs.ErrNotExist):
		if chainErr != nil {
			return nil, fmt.Errorf("%w: there is no state, and the copies do not form a valid chain: %v", errInconsistent, chainErr)
		}
		r.state = &State{Version: stateVersion}
		if chain != nil {
			r.state.Chain = rebuildChainState(chain)
			description := fmt.Sprintf("rebuilding the missing state from the chain %s", chain.ID())
			if err := fix(description, func() error { return writeState(a.layout.state(), r.state) }); err != nil {
				return nil, err
			}
		}

	default:
		if chainErr != nil {
			return nil, fmt.Errorf("%w: %v, and the copies do not form a valid chain: %v", errInconsistent, stateErr, chainErr)
		}
		r.state = &State{Version: stateVersion}
		description := fmt.Sprintf("%v: keeping it aside and starting with an empty state", stateErr)
		if chain != nil {
			r.state.Chain = rebuildChainState(chain)
			description = fmt.Sprintf("%v: keeping it aside and rebuilding the state from the chain %s", stateErr, chain.ID())
		}
		if err := fix(description, func() error { return a.replaceCorruptState(r.state) }); err != nil {
			return nil, err
		}
	}

	r.chain = chain
	return r, nil
}

// matchState compares the state with the chain on disk. The state may lag
// behind by the last copy: a copy is published before the state is written,
// and a run that stops in between leaves a verified copy the state does not
// know yet.
func matchState(state *ChainState, chain *Chain) (lagging bool, err error) {
	if state.ID != chain.ID() {
		return false, fmt.Errorf("the state records the chain %s, but the copies on disk are of the chain %s", state.ID, chain.ID())
	}
	if state.FullPath != statePath(chain.Full().Name) {
		return false, fmt.Errorf("the state records the full copy %s, but the full copy on disk is %s", state.FullPath, statePath(chain.Full().Name))
	}

	index := -1
	for i, item := range chain.Copies {
		if statePath(item.Name) == state.LastPath {
			index = i
		}
	}
	if index < 0 {
		return false, fmt.Errorf("the last copy %s of the state is not on disk", state.LastPath)
	}

	last := chain.Copies[index]
	if last.Checkpoints.FromLSN != state.LastFromLSN || last.Checkpoints.ToLSN != state.LastToLSN {
		return false, fmt.Errorf("the LSN of %s differ: %d-%d in the state, %d-%d on disk",
			state.LastPath, state.LastFromLSN, state.LastToLSN, last.Checkpoints.FromLSN, last.Checkpoints.ToLSN)
	}

	switch newer := len(chain.Copies) - 1 - index; newer {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, fmt.Errorf("%d copies on disk are newer than the last copy %s of the state", newer, state.LastPath)
	}
}

// rebuildChainState restores the state of a valid chain from the copies. The
// start times come from the names, the finish time from the archive.
func rebuildChainState(chain *Chain) *ChainState {
	full, last := chain.Full(), chain.Last()
	state := ChainState{
		ID:            chain.ID(),
		FullStartedAt: full.Name.started,
		FullPath:      statePath(full.Name),
	}
	return state.withLast(last, last.Name.started, last.ModTime)
}

// replaceCorruptState keeps the corrupt state next to the new one for a later look
func (a *App) replaceCorruptState(state *State) error {
	corrupt := a.layout.state() + ".corrupt-" + a.now().UTC().Format(incTagLayout)
	if err := os.Rename(a.layout.state(), corrupt); err != nil {
		return err
	}
	a.log.Warn("the corrupt state is kept", "path", corrupt)
	return writeState(a.layout.state(), state)
}

type mode int

const (
	modeFull mode = iota
	modeIncremental
	modeRotate
)

func (m mode) String() string {
	switch m {
	case modeFull:
		return "full"
	case modeIncremental:
		return "incremental"
	default:
		return "rotation"
	}
}

// decide picks what the run does. The full copy is renewed once
// fullIntervalDays calendar days have passed since the day it was started:
// the time of the day is ignored, so a full copy started late does not push
// the next one by a day.
func decide(state *State, now time.Time, location *time.Location, intervalDays int) mode {
	if state.Chain == nil {
		return modeFull
	}
	if calendarDays(state.Chain.FullStartedAt, now, location) >= intervalDays {
		return modeRotate
	}
	return modeIncremental
}

// calendarDays returns how many calendar days of the location lie between the two moments
func calendarDays(from, to time.Time, location *time.Location) int {
	fromYear, fromMonth, fromDay := from.In(location).Date()
	toYear, toMonth, toDay := to.In(location).Date()
	// Midnights of UTC dates are exactly 24 hours apart, unlike the local
	// ones on the days the clocks are changed
	start := time.Date(fromYear, fromMonth, fromDay, 0, 0, 0, 0, time.UTC)
	end := time.Date(toYear, toMonth, toDay, 0, 0, 0, 0, time.UTC)
	return int(end.Sub(start).Hours() / 24)
}
