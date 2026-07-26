// Package logging provides the independent JSON log streams of rukh.
//
// Observability is reading log files: no dashboard, no metrics export,
// no rotation logic in the binary. Rotation is delegated to logrotate,
// which sends SIGHUP; the daemon then calls Reopen on every stream.
package logging

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ostap-mykhaylyak/rukh/internal/paths"
)

// flushInterval is how long a buffered line can wait before reaching
// the disk. It bounds what a crash could lose from the access log.
const flushInterval = 250 * time.Millisecond

// bufferSize is the access log buffer. A JSON request line is ~350
// bytes, so this is roughly a hundred requests per write(2).
const bufferSize = 32 << 10

// stream is a log file that can be reopened in place (logrotate hook).
// Writes are serialized with a mutex so a reopen never races a line.
type stream struct {
	mu   sync.Mutex
	path string
	f    *os.File
	// bw is set for the access stream only: one write syscall per
	// request, serialized on this mutex, is the single most expensive
	// thing rukh does per request — and an access line is not worth
	// paying it for. Buffering costs at most flushInterval of lines if
	// the daemon is killed.
	bw *bufio.Writer
}

func openStream(path string, buffered bool) (*stream, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, err
	}
	s := &stream{path: path, f: f}
	if buffered {
		s.bw = bufio.NewWriterSize(f, bufferSize)
	}
	return s, nil
}

func (s *stream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bw != nil {
		return s.bw.Write(p)
	}
	return s.f.Write(p)
}

// Flush pushes buffered lines to the file. Called by the flusher
// goroutine, and before every reopen or close.
func (s *stream) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *stream) flushLocked() error {
	if s.bw == nil {
		return nil
	}
	return s.bw.Flush()
}

func (s *stream) Reopen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The buffer must reach the OLD file: those lines belong to the
	// rotated one.
	if err := s.flushLocked(); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	old := s.f
	s.f = f
	if s.bw != nil {
		s.bw.Reset(f)
	}
	return old.Close()
}

func (s *stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.flushLocked()
	if cerr := s.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Streams holds one independent *slog.Logger per concern.
type Streams struct {
	Service *slog.Logger // daemon lifecycle, config reloads, nginx discovery
	Access  *slog.Logger // one line per proxied request
	Learn   *slog.Logger // early hints, prefetch and preload decisions

	files []*stream
	stop  chan struct{}
	once  sync.Once
}

// Open opens all log streams under dir (production: paths.LogDir).
func Open(dir string) (*Streams, error) {
	s := &Streams{stop: make(chan struct{})}
	for _, def := range []struct {
		name     string
		dst      **slog.Logger
		buffered bool
	}{
		// The service and learn streams stay unbuffered: they are rare
		// and they are what an operator reads while debugging, so they
		// must be on disk the moment they happen.
		{paths.ServiceLog, &s.Service, false},
		{paths.AccessLog, &s.Access, true},
		{paths.LearnLog, &s.Learn, false},
	} {
		f, err := openStream(filepath.Join(dir, def.name), def.buffered)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.files = append(s.files, f)
		*def.dst = slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

	go func() {
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				for _, f := range s.files {
					f.Flush()
				}
			}
		}
	}()
	return s, nil
}

// Discard returns streams writing nowhere (tests).
func Discard() *Streams {
	l := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return &Streams{Service: l, Access: l, Learn: l, stop: make(chan struct{})}
}

// Reopen reopens every log file. Called on SIGHUP (logrotate hook).
func (s *Streams) Reopen() error {
	var errs []error
	for _, f := range s.files {
		if err := f.Reopen(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Close flushes and closes every log file. Called on shutdown.
func (s *Streams) Close() error {
	s.once.Do(func() { close(s.stop) })
	var errs []error
	for _, f := range s.files {
		if err := f.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
