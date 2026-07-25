package hints

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"

	"github.com/ostap-mykhaylyak/rukh/internal/learn"
)

// Store holds the compiled hints files behind an atomic pointer, so a
// lookup on the request path is a map read with no lock.
//
// A file that stops parsing keeps serving its last good version: an
// operator editing a file at the wrong moment must not silently turn
// the hints off.
type Store struct {
	dir string
	log *slog.Logger

	cur   atomic.Pointer[index]
	files atomic.Pointer[[]Info]
	w     *fsnotify.Watcher
}

// index is the immutable lookup table.
type index struct {
	byHost   map[string]*Host
	wildcard []wildHost // "*.example.com" entries, longest suffix first
}

type wildHost struct {
	suffix string // ".example.com"
	host   *Host
}

// Info describes one file, for `rukh status` and --check-config.
type Info struct {
	File     string   `json:"file"`
	Hosts    []string `json:"hosts"`
	Entries  int      `json:"entries"`
	Error    string   `json:"error,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// NewStore returns a Store reading *.yaml from dir.
func NewStore(dir string, log *slog.Logger) *Store {
	s := &Store{dir: dir, log: log}
	s.cur.Store(&index{byHost: map[string]*Host{}})
	empty := []Info{}
	s.files.Store(&empty)
	return s
}

// LoadAll re-reads every file in the directory. A missing directory is
// not an error: manual hints are optional.
func (s *Store) LoadAll() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if !os.IsNotExist(err) {
			s.log.Error("cannot read the hints directory", "dir", s.dir, "error", err)
		}
		return
	}

	prev := s.cur.Load()
	next := &index{byHost: map[string]*Host{}}
	var infos []Info

	for _, e := range entries {
		if e.IsDir() || !isHintsFile(e.Name()) {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		info := Info{File: e.Name()}
		name := hostFromFile(e.Name())

		data, err := os.ReadFile(path)
		if err != nil {
			info.Error = err.Error()
			s.keepLastGood(prev, next, name, &info)
			infos = append(infos, info)
			s.log.Error("cannot read hints file", "file", path, "error", err)
			continue
		}
		host, warnings, err := Parse(name, data)
		if err != nil {
			info.Error = err.Error()
			s.keepLastGood(prev, next, name, &info)
			infos = append(infos, info)
			s.log.Error("invalid hints file, keeping the last good version", "file", path, "error", err)
			continue
		}
		info.Hosts = host.Hosts
		info.Entries = host.Count
		info.Warnings = warnings
		for _, w := range warnings {
			s.log.Warn("hints file warning", "file", path, "warning", w)
		}
		for _, h := range host.Hosts {
			addHost(next, h, host)
		}
		infos = append(infos, info)
	}

	sort.Slice(next.wildcard, func(i, j int) bool {
		return len(next.wildcard[i].suffix) > len(next.wildcard[j].suffix)
	})
	sort.Slice(infos, func(i, j int) bool { return infos[i].File < infos[j].File })
	s.cur.Store(next)
	s.files.Store(&infos)
}

// keepLastGood carries a broken file's previous compilation over.
func (s *Store) keepLastGood(prev, next *index, name string, info *Info) {
	old, ok := prev.byHost[name]
	if !ok {
		return
	}
	addHost(next, name, old)
	info.Hosts = []string{name}
	info.Entries = old.Count
}

func addHost(idx *index, name string, host *Host) {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if strings.HasPrefix(name, "*.") {
		idx.wildcard = append(idx.wildcard, wildHost{suffix: name[1:], host: host})
		return
	}
	idx.byHost[name] = host
}

// Lookup returns the configured hints for a host and path.
func (s *Store) Lookup(host, path string) []learn.Hint {
	idx := s.cur.Load()
	if len(idx.byHost) == 0 && len(idx.wildcard) == 0 {
		return nil
	}
	host = strings.ToLower(host)
	if h, ok := idx.byHost[host]; ok {
		return h.Lookup(path)
	}
	for _, w := range idx.wildcard {
		if strings.HasSuffix(host, w.suffix) {
			return w.host.Lookup(path)
		}
	}
	return nil
}

// Snapshot returns what is loaded, for the status socket.
func (s *Store) Snapshot() []Info { return *s.files.Load() }

// Count returns the number of hosts covered by a file.
func (s *Store) Count() int {
	idx := s.cur.Load()
	return len(idx.byHost) + len(idx.wildcard)
}

// Watch reloads the directory whenever it changes, until stop is
// closed. Editors write via temporary files and rename, so the whole
// directory is watched and reloaded as a unit.
func (s *Store) Watch(stop <-chan struct{}) error {
	if _, err := os.Stat(s.dir); os.IsNotExist(err) {
		return nil // nothing to watch; created by --init
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("hints watch: %w", err)
	}
	if err := w.Add(s.dir); err != nil {
		w.Close()
		return fmt.Errorf("hints watch: %w", err)
	}
	s.w = w
	go func() {
		defer w.Close()
		for {
			select {
			case <-stop:
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if !isHintsFile(filepath.Base(ev.Name)) {
					continue
				}
				s.LoadAll()
				s.log.Info("hints reloaded", "hosts", s.Count(), "trigger", filepath.Base(ev.Name))
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				s.log.Error("hints watch error", "error", err)
			}
		}
	}()
	return nil
}

// isHintsFile keeps the .example files shipped by --init out of the
// way, and ignores editor leftovers.
func isHintsFile(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	ext := filepath.Ext(name)
	return ext == ".yaml" || ext == ".yml"
}

func hostFromFile(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
}
