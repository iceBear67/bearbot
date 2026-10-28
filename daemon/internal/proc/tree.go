package proc

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// Entry identifies one process read from /proc. StartTime (field 22 of /proc/<pid>/stat) tells a process apart
// from a later process that reuses the same PID.
type Entry struct {
	PID       int
	PPID      int
	StartTime uint64
	State     byte
}

// ReadEntry reads /proc/<pid>/stat. It returns os.ErrNotExist (wrapped) when the process does not exist.
func ReadEntry(pid int) (Entry, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return Entry{}, fmt.Errorf("process %d: %w", pid, os.ErrNotExist)
		}
		return Entry{}, err
	}
	return parseStat(pid, data)
}

func parseStat(pid int, data []byte) (Entry, error) {
	// The command name is in parentheses and may itself contain spaces and parentheses.
	end := bytes.LastIndexByte(data, ')')
	if end < 0 || end+2 > len(data) {
		return Entry{}, fmt.Errorf("unexpected /proc/%d/stat content %q", pid, data)
	}
	fields := bytes.Fields(data[end+2:])
	// fields[0] is field 3 (state), fields[1] field 4 (ppid), fields[19] field 22 (starttime).
	if len(fields) < 20 {
		return Entry{}, fmt.Errorf("unexpected /proc/%d/stat content %q", pid, data)
	}
	ppid, err := strconv.Atoi(string(fields[1]))
	if err != nil {
		return Entry{}, fmt.Errorf("parse ppid in /proc/%d/stat: %w", pid, err)
	}
	start, err := strconv.ParseUint(string(fields[19]), 10, 64)
	if err != nil {
		return Entry{}, fmt.Errorf("parse starttime in /proc/%d/stat: %w", pid, err)
	}
	return Entry{PID: pid, PPID: ppid, StartTime: start, State: fields[0][0]}, nil
}

// Snapshot reads every process of /proc. Processes that exit while the directory is read are skipped.
func Snapshot() ([]Entry, error) {
	dir, err := os.Open("/proc")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(names))
	for _, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		e, err := ReadEntry(pid)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// CollectTree returns the process root followed by all of its descendants (breadth first), as found in /proc at
// the time of the call. It returns an empty list when root does not exist.
func CollectTree(root int) ([]Entry, error) {
	all, err := Snapshot()
	if err != nil {
		return nil, err
	}
	children := make(map[int][]Entry)
	var rootEntry *Entry
	for i := range all {
		e := all[i]
		if e.PID == root {
			rootEntry = &all[i]
		}
		children[e.PPID] = append(children[e.PPID], e)
	}
	if rootEntry == nil {
		return nil, nil
	}
	tree := []Entry{*rootEntry}
	seen := map[int]bool{root: true}
	for i := 0; i < len(tree); i++ {
		for _, c := range children[tree[i].PID] {
			if !seen[c.PID] {
				seen[c.PID] = true
				tree = append(tree, c)
			}
		}
	}
	return tree, nil
}

// Alive reports whether the process e still exists (same PID and start time) and is not a zombie.
func (e Entry) Alive() (bool, error) {
	cur, err := ReadEntry(e.PID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return cur.StartTime == e.StartTime && cur.State != 'Z', nil
}

// Signal sends sig to the process e if it is still the same process. It uses a pidfd, so a PID that was reused by
// another process is never signalled. A process that is gone is not an error.
func (e Entry) Signal(sig syscall.Signal) error {
	fd, err := unix.PidfdOpen(e.PID, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		return fmt.Errorf("pidfd_open %d: %w", e.PID, err)
	}
	defer unix.Close(fd)
	cur, err := ReadEntry(e.PID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if cur.StartTime != e.StartTime {
		return nil
	}
	if err := unix.PidfdSendSignal(fd, sig, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("signal %v to %d: %w", sig, e.PID, err)
	}
	return nil
}

// KillAll sends SIGKILL to every listed process that is still alive.
func KillAll(entries []Entry) error {
	var errs []error
	for _, e := range entries {
		if err := e.Signal(syscall.SIGKILL); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func mergeEntries(lists ...[]Entry) []Entry {
	var out []Entry
	for _, l := range lists {
		for _, e := range l {
			if !slices.ContainsFunc(out, func(o Entry) bool { return o.PID == e.PID && o.StartTime == e.StartTime }) {
				out = append(out, e)
			}
		}
	}
	return out
}
