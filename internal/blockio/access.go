/*
 * Copyright 2024 the urpc project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package blockio

import (
	"sync"
	"sync/atomic"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/readcontrol"
)

// DefaultWindow is the default per-direction blocking buffer limit (64 KiB).
const DefaultWindow = 64 << 10

// Source abstracts the non-blocking stream connection operations needed by
// Reader and Writer without importing root package uio.
type Source interface {
	PeekChunk() []byte
	Discard(n int) (int, error)
	InboundBuffered() int
	ReadEnded() bool
	WriteEnded() bool
	ClosingOrClosed() bool

	Write(p []byte) (int, error)
	Writev(vec [][]byte) (int, error)
	WriteOwned(buf *bytebuf.Buffer) (int, error)
	Flush() error
	OutboundBuffered() int

	MaxInbound() int
	MaxOutbound() int
	EnablePullRead() (onConnTurn bool, err error)
	Wake() error
	NewReadPause() *readcontrol.Pause
	WatchWrite(fn func(int)) bool
	IsOverflow(err error) bool
}

// State lazily holds a stream connection's Reader and Writer instances.
type State struct {
	mu         sync.Mutex
	src        Source
	reader     atomic.Pointer[Reader]
	writer     atomic.Pointer[Writer]
	closed     bool
	localClose bool
	closeErr   error
}

// NewState creates a new blocking I/O attachment state for src.
func NewState(src Source) *State {
	return &State{src: src}
}

// Reader returns the connection's single blocking Reader, creating it and
// switching inbound delivery to pull mode on the first call.
func (s *State) Reader(window int) (*Reader, error) {
	s.mu.Lock()
	if r := s.reader.Load(); r != nil {
		s.mu.Unlock()
		if window > 0 {
			r.setWindow(window)
		}
		return r, nil
	}
	r := newReader(s.src, window)
	if s.closed {
		r.onClose(s.localClose, s.closeErr)
	} else if s.src.ClosingOrClosed() {
		r.onClose(false, nil)
	}
	s.reader.Store(r)
	s.mu.Unlock()

	onConnTurn, err := s.src.EnablePullRead()
	if err != nil {
		return nil, err
	}
	if onConnTurn {
		r.Pump()
	} else if s.src.ClosingOrClosed() {
		s.OnClose(false, nil)
	}
	return r, nil
}

// Writer returns the connection's single blocking Writer, creating it and
// subscribing to write-progress notifications on the first call.
func (s *State) Writer(window int) *Writer {
	s.mu.Lock()
	if w := s.writer.Load(); w != nil {
		s.mu.Unlock()
		if window > 0 {
			w.setWindow(window)
		}
		return w
	}
	w := newWriter(s.src, window)
	if s.closed {
		w.onClose(s.localClose, s.closeErr)
	}
	s.writer.Store(w)
	s.mu.Unlock()

	s.src.WatchWrite(func(int) { w.NotifySpace() })
	return w
}

// ActiveReader returns the attached Reader if pull mode is active, or nil.
func (s *State) ActiveReader() *Reader {
	if s == nil {
		return nil
	}
	return s.reader.Load()
}

// HasWriter reports whether a blocking Writer has been attached to s.
func (s *State) HasWriter() bool {
	return s != nil && s.writer.Load() != nil
}

// OnClose notifies any attached Reader and Writer that the underlying
// connection closed. localClose is true when closed locally via Conn.Close().
func (s *State) OnClose(localClose bool, err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.localClose = localClose
	s.closeErr = err
	r := s.reader.Load()
	w := s.writer.Load()
	s.mu.Unlock()

	if r != nil {
		r.onClose(localClose, err)
	}
	if w != nil {
		w.onClose(localClose, err)
	}
}

// Getter resolves the State of a concrete connection implementation.
type Getter interface {
	GetBlockState(source any) (*State, bool)
}

// Access embeds a zero-sized capability marker on supported connections.
type Access[T Getter] struct{}

func (Access[T]) blockStateBoundary(source any) (*State, bool) {
	var getter T
	return getter.GetBlockState(source)
}

// StateOf resolves source's blocking I/O State, or (nil, false) if unsupported
// (such as datagram connections).
func StateOf(source any) (*State, bool) {
	if access, ok := source.(interface{ blockStateBoundary(any) (*State, bool) }); ok {
		return access.blockStateBoundary(source)
	}
	return nil, false
}
