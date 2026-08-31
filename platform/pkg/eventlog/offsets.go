package eventlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/usslp/usslp/platform/pkg/obs"
)

// offsetsDirName is the reserved directory holding committed consumer offsets.
// It sits inside the log directory rather than beside it so that a whole
// environment — data and consumer state — is one path to back up, copy or
// throw away.
const offsetsDirName = "__offsets"

// tp identifies one topic-partition.
type tp struct {
	topic     string
	partition int
}

func (t tp) String() string { return t.topic + "/" + fmt.Sprint(t.partition) }

// offsetStore holds and persists the committed position of every consumer
// group.
//
// Commits are held in memory and flushed asynchronously. That is a deliberate
// weakening: forcing an fsync per commit would cost more than the append it
// follows, and it buys nothing, because delivery is at-least-once either way.
// A crash between a commit and a flush re-delivers records the handler already
// processed — which handlers must tolerate regardless, since a crash between
// the handler returning and the commit has exactly the same effect.
type offsetStore struct {
	dir    string
	lg     *obs.Logger
	mu     sync.Mutex
	groups map[string]map[tp]int64
	dirty  map[string]bool
	flush  chan struct{}
}

type offsetFile struct {
	Group   string        `json:"group"`
	Entries []offsetEntry `json:"offsets"`
}

type offsetEntry struct {
	Topic     string `json:"topic"`
	Partition int    `json:"partition"`
	Offset    int64  `json:"offset"`
}

// openOffsetStore loads every persisted group from dir.
func openOffsetStore(dir string, lg *obs.Logger) (*offsetStore, error) {
	s := &offsetStore{
		dir:    dir,
		lg:     lg,
		groups: make(map[string]map[tp]int64),
		dirty:  make(map[string]bool),
		flush:  make(chan struct{}, 1),
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var f offsetFile
		if err := json.Unmarshal(b, &f); err != nil {
			// A half-written offsets file loses commits, not data: the group
			// simply re-reads from wherever it last flushed cleanly. Refusing
			// to open the whole log over it would be a worse outcome.
			lg.Warn("eventlog: discarding unreadable offsets file", "file", e.Name(), "error", err)
			continue
		}
		m := make(map[tp]int64, len(f.Entries))
		for _, ent := range f.Entries {
			m[tp{ent.Topic, ent.Partition}] = ent.Offset
		}
		s.groups[f.Group] = m
	}
	return s, nil
}

// committed returns the stored position for a group's partition.
func (s *offsetStore) committed(group string, t tp) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[group]
	if !ok {
		return 0, false
	}
	off, ok := g[t]
	return off, ok
}

// resolveStart returns the position a member should start from, calling start
// only if the group has never had a position for this partition.
//
// Resolving once and remembering the answer is what stops a rebalance from
// skipping records: a group configured to start at the tail must pin that tail
// the first time it sees a partition, or every subsequent owner would re-read
// "the tail" as it is then and silently drop everything produced in between.
func (s *offsetStore) resolveStart(group string, t tp, start func() int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[group]
	if !ok {
		g = make(map[tp]int64)
		s.groups[group] = g
	}
	if off, ok := g[t]; ok {
		return off
	}
	off := start()
	g[t] = off
	s.dirty[group] = true
	s.signalLocked()
	return off
}

// commit records a group's new position. It never moves a position backwards:
// a late commit from a member that has already been rebalanced away must not
// undo the progress of the partition's new owner.
func (s *offsetStore) commit(group string, t tp, offset int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[group]
	if !ok {
		g = make(map[tp]int64)
		s.groups[group] = g
	}
	if cur, ok := g[t]; ok && offset <= cur {
		return
	}
	g[t] = offset
	s.dirty[group] = true
	s.signalLocked()
}

func (s *offsetStore) signalLocked() {
	select {
	case s.flush <- struct{}{}:
	default:
	}
}

// flushed reports the channel that signals pending work to the flusher.
func (s *offsetStore) pending() <-chan struct{} { return s.flush }

// Flush writes every group with uncommitted changes to disk.
func (s *offsetStore) Flush() error {
	s.mu.Lock()
	groups := make([]string, 0, len(s.dirty))
	for g, d := range s.dirty {
		if d {
			groups = append(groups, g)
		}
	}
	snapshots := make(map[string]offsetFile, len(groups))
	for _, g := range groups {
		f := offsetFile{Group: g}
		for t, off := range s.groups[g] {
			f.Entries = append(f.Entries, offsetEntry{Topic: t.topic, Partition: t.partition, Offset: off})
		}
		sort.Slice(f.Entries, func(i, j int) bool {
			if f.Entries[i].Topic != f.Entries[j].Topic {
				return f.Entries[i].Topic < f.Entries[j].Topic
			}
			return f.Entries[i].Partition < f.Entries[j].Partition
		})
		snapshots[g] = f
		delete(s.dirty, g)
	}
	s.mu.Unlock()

	var first error
	for g, f := range snapshots {
		b, err := json.Marshal(f)
		if err == nil {
			err = writeFileAtomic(filepath.Join(s.dir, offsetFileName(g)), b)
		}
		if err != nil {
			// Put the group back on the dirty list so the next flush retries;
			// silently dropping it would strand the group's progress.
			s.mu.Lock()
			s.dirty[g] = true
			s.mu.Unlock()
			if first == nil {
				first = err
			}
		}
	}
	return first
}

// offsetFileName maps a group id to a filename, escaping anything that is not
// unambiguously safe in a path. Group ids come from configuration and can
// legitimately contain slashes and dots; a group called "../../etc" must become
// a file, not an escape.
func offsetFileName(group string) string {
	var b strings.Builder
	for i := 0; i < len(group); i++ {
		c := group[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String() + ".json"
}
