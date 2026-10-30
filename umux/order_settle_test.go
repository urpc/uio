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

package umux

import (
	"sync"
	"testing"
	"time"

	"github.com/urpc/uio"
)

// quietWire accepts frames and reports the headers it saw.
type quietWire struct {
	uio.Conn
	mu     sync.Mutex
	frames []frameHeader
}

func (*quietWire) Wake() error                     { return nil }
func (*quietWire) SetWakeDeadline(time.Time) error { return nil }
func (*quietWire) Write(p []byte) (int, error)     { return len(p), nil }

func (c *quietWire) WriteOwned(b *uio.Buffer) (int, error) {
	h, err := decodeHeader(b.Bytes()[:headerSize])
	if err != nil {
		panic(err)
	}
	c.mu.Lock()
	c.frames = append(c.frames, h)
	c.mu.Unlock()
	n := b.Len()
	uio.ReleaseBuffer(b)
	return n, nil
}

func (c *quietWire) seen() []frameHeader {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]frameHeader(nil), c.frames...)
}

func quietPair() (*Session, *Stream, *quietWire) {
	wire := &quietWire{}
	s := &Session{
		cfg:      Config{KeepAlive: -1}.resolve(),
		conn:     wire,
		streams:  make(map[uint32]*Stream),
		openWait: make(map[uint32]chan error),
	}
	st := newStream(s, 1, false, int64(s.cfg.WindowSize))
	s.streams[1] = st
	return s, st, wire
}

// TestStreamDiscardSettlesFullyEnded: discarding the last unread bytes of a
// stream whose directions both ended must settle it, exactly as a read does.
func TestStreamDiscardSettlesFullyEnded(t *testing.T) {
	s, st, _ := quietPair()
	if err := st.pushData([]byte("x"), true, false); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	s.flush()
	if _, err := st.Discard(-1); err != nil {
		t.Fatal(err)
	}
	if s.streams[st.id] != nil {
		t.Fatalf("discard left fully ended stream in map; state=%v", st.State())
	}
}

// TestStreamWriteBeforeFINOrdered: a Write that succeeded must put all of its
// data on the wire before the FIN a racing CloseWrite sends.
func TestStreamWriteBeforeFINOrdered(t *testing.T) {
	for attempt := 0; attempt < 2000; attempt++ {
		s, st, wire := quietPair()
		s.cfg.MaxFrame = 1
		payload := make([]byte, 16)
		var wg sync.WaitGroup
		wg.Add(2)
		var accepted int
		var werr error
		go func() {
			defer wg.Done()
			accepted, werr = st.Write(payload)
		}()
		go func() {
			defer wg.Done()
			_ = st.CloseWrite()
		}()
		wg.Wait()
		s.flush()
		s.mu.Lock()
		st.dropOutLocked()
		s.mu.Unlock()
		fin := false
		data := 0
		for _, h := range wire.seen() {
			if h.flags&flagFIN != 0 {
				fin = true
				continue
			}
			if h.typ == typeData && h.length > 0 {
				if fin {
					t.Fatalf("attempt %d: data after FIN: %+v", attempt, wire.seen())
				}
				data += int(h.length)
			}
		}
		if data != accepted || (werr == nil && accepted != len(payload)) {
			t.Fatalf("attempt %d: %d payload bytes on the wire, write accepted %d (err %v)", attempt, data, accepted, werr)
		}
	}
}
