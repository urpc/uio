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
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urpc/uio"
)

func waitStreamError(t *testing.T, st *Stream) error {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	buf := make([]byte, 1)
	for time.Now().Before(deadline) {
		_, err := st.Read(buf)
		if err != nil {
			return err
		}
		time.Sleep(200 * time.Microsecond)
	}
	t.Fatal("stream did not close before the deadline")
	return nil
}

func TestStreamReadDeadlineResetsStream(t *testing.T) {
	sess, echo := startPair(t, Config{KeepAlive: -1}, Config{KeepAlive: -1})
	st, err := sess.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetReadDeadline(time.Now().Add(40 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	readErr := waitStreamError(t, st)
	if !errors.Is(readErr, os.ErrDeadlineExceeded) {
		t.Fatalf("Read error = %v, want deadline exceeded", readErr)
	}
	if err := st.SetReadDeadline(time.Time{}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("SetReadDeadline after expiry = %v, want net.ErrClosed", err)
	}
	select {
	case closeErr := <-echo.closed:
		if !errors.Is(closeErr, ErrStreamReset) {
			t.Fatalf("remote close = %v, want ErrStreamReset", closeErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remote stream did not observe the reset")
	}
}

func TestStreamWriteDeadlineResetsStream(t *testing.T) {
	sess, _ := startPair(t, Config{KeepAlive: -1}, Config{KeepAlive: -1})
	st, err := sess.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetWriteDeadline(time.Now().Add(40 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	writeErr := waitStreamError(t, st)
	if !errors.Is(writeErr, os.ErrDeadlineExceeded) {
		t.Fatalf("Read after write deadline = %v, want deadline exceeded", writeErr)
	}
	if !strings.Contains(writeErr.Error(), "write deadline") {
		t.Fatalf("Read error = %v, want write deadline cause", writeErr)
	}
}

func TestStreamDeadlinesAreIndependentAndClearable(t *testing.T) {
	sess, _ := startPair(t, Config{KeepAlive: -1}, Config{KeepAlive: -1})
	st, err := sess.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetWriteDeadline(time.Now().Add(40 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	// Updating the read deadline must not clear the write deadline.
	if err := st.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	writeErr := waitStreamError(t, st)
	if !errors.Is(writeErr, os.ErrDeadlineExceeded) || !strings.Contains(writeErr.Error(), "write deadline") {
		t.Fatalf("Read error = %v, want preserved write deadline", writeErr)
	}

	sess2, _ := startPair(t, Config{KeepAlive: -1}, Config{KeepAlive: -1})
	st2, err := sess2.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := st2.SetReadDeadline(time.Now().Add(40 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := st2.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if st2.State()&uio.StateClosed != 0 {
		t.Fatal("cleared read deadline still closed the stream")
	}
	_ = st2.CloseWith(errors.New("test complete"))
}

func TestStreamDeadlineDropsQueuedDataBeforeReset(t *testing.T) {
	s, st, wire, h := productionStreamPair(t)
	if _, err := st.Write([]byte("queued")); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeadline(time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.onTimer(wire); err != nil {
		t.Fatal(err)
	}
	frames := wire.sent()
	if len(frames) != 0 {
		t.Fatalf("frames = %+v, want no data frame after deadline reset", frames)
	}
	if _, ok := s.streams[st.id]; ok {
		t.Fatal("deadline reset left the stream in the session")
	}
	closes := h.closed()
	if len(closes) != 1 || !errors.Is(closes[0], os.ErrDeadlineExceeded) {
		t.Fatalf("OnClose = %v, want one deadline error", closes)
	}
}
