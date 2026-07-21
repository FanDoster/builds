package runner

import (
	"bytes"
	"net/url"
	"sync"
	"time"

	"github.com/FanDoster/builds/internal/db"
	"github.com/FanDoster/builds/internal/logbus"
)

const (
	// sinkFlushInterval / sinkFlushBytes bound how far the DB row can lag
	// behind the live bus stream.
	sinkFlushInterval = 500 * time.Millisecond
	sinkFlushBytes    = 8 * 1024
	// sinkMaxHold caps how long a line can grow before we force-emit it.
	sinkMaxHold = 8 * 1024
)

// logSink is an io.Writer that streams command output into the log bus
// immediately and into the DB in batches. Secrets are scrubbed before any
// byte leaves the sink; scanning happens on complete \n- or \r-terminated
// segments (a secret cannot contain either), with a holdback window when a
// force-drain splits an unfinished line.
type logSink struct {
	buildID int64
	secret  string
	encSec  string
	db      *db.DB
	bus     *logbus.Bus

	mu        sync.Mutex
	lineBuf   []byte // incomplete trailing segment, not yet scrubbed/emitted
	dbBuf     []byte // scrubbed bytes not yet flushed to the DB
	lastFlush time.Time
	closed    bool
	stop      chan struct{}
}

func newLogSink(buildID int64, secret string, database *db.DB, bus *logbus.Bus) *logSink {
	s := &logSink{
		buildID:   buildID,
		secret:    secret,
		db:        database,
		bus:       bus,
		lastFlush: time.Now(),
		stop:      make(chan struct{}),
	}
	if secret != "" {
		s.encSec = url.User(secret).String()
	}
	// Background flusher covers quiet periods (slow command, no output).
	go func() {
		ticker := time.NewTicker(sinkFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				s.mu.Lock()
				s.flushDBLocked()
				s.mu.Unlock()
			}
		}
	}()
	return s
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return len(p), nil
	}
	s.lineBuf = append(s.lineBuf, p...)

	// Emit everything up to and including the last segment terminator.
	if i := lastTerminator(s.lineBuf); i >= 0 {
		s.emitLocked(s.lineBuf[:i+1])
		s.lineBuf = append(s.lineBuf[:0], s.lineBuf[i+1:]...)
	}

	// Force-drain an overlong unterminated line, holding back a window large
	// enough that a straddling secret is still caught on the next emit.
	if len(s.lineBuf) > sinkMaxHold {
		hold := s.holdback()
		cut := len(s.lineBuf) - hold
		if cut > 0 {
			s.emitLocked(s.lineBuf[:cut])
			s.lineBuf = append(s.lineBuf[:0], s.lineBuf[cut:]...)
		}
	}

	if len(s.dbBuf) >= sinkFlushBytes || time.Since(s.lastFlush) >= sinkFlushInterval {
		s.flushDBLocked()
	}
	return len(p), nil
}

// holdback is the number of trailing bytes that must stay unscanned so a
// secret split across a forced drain cannot leak.
func (s *logSink) holdback() int {
	if s.secret == "" {
		return 0
	}
	h := len(s.secret)
	if len(s.encSec) > h {
		h = len(s.encSec)
	}
	return h
}

// emitLocked scrubs a chunk and hands it to the bus (immediately) and the DB
// buffer (batched).
func (s *logSink) emitLocked(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	scrubbed := scrubSecret(string(chunk), s.secret)
	s.bus.Publish(s.buildID, []byte(scrubbed))
	s.dbBuf = append(s.dbBuf, scrubbed...)
}

func (s *logSink) flushDBLocked() {
	if len(s.dbBuf) == 0 {
		s.lastFlush = time.Now()
		return
	}
	s.db.AppendBuildLog(s.buildID, string(s.dbBuf))
	s.dbBuf = s.dbBuf[:0]
	s.lastFlush = time.Now()
}

// Close drains any partial line and flushes the DB buffer. Idempotent.
func (s *logSink) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.stop)
	if len(s.lineBuf) > 0 {
		s.emitLocked(s.lineBuf)
		s.lineBuf = nil
	}
	s.flushDBLocked()
}

// lastTerminator returns the index of the last \n or \r in b, or -1.
func lastTerminator(b []byte) int {
	n := bytes.LastIndexByte(b, '\n')
	r := bytes.LastIndexByte(b, '\r')
	if r > n {
		return r
	}
	return n
}
