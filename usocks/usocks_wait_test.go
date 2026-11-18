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

package usocks

import (
	"testing"
	"time"
)

// Wait joins the relay even when the main engine's lifecycle ends without
// Server.Close: a Serve that fails to initialize must not leave Wait hanging on
// a relay nothing else will close.
func TestWaitAfterServeError(t *testing.T) {
	srv := NewServer(Options{})
	if err := srv.Serve("tcp://256.256.256.256:1"); err == nil {
		t.Fatal("expected a Serve error for an invalid address")
	}
	done := make(chan struct{})
	go func() { _ = srv.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait hung after a Serve error (relay not joined)")
	}
}

// Stopping through the Events() escape hatch (rather than Server.Close) must
// also stop the relay, so Wait returns and the relay loops do not leak.
func TestWaitAfterEventsClose(t *testing.T) {
	srv := NewServer(Options{})
	go srv.Events().Serve()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := srv.Listen("tcp://127.0.0.1:0"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("listen never ready")
		}
		time.Sleep(time.Millisecond)
	}
	_ = srv.Events().Close(nil)
	done := make(chan struct{})
	go func() { _ = srv.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait hung after Events().Close (relay not joined)")
	}
}
