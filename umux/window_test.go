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
	"errors"
	"testing"
)

func TestSendWindowTakeAndGrant(t *testing.T) {
	w := newSendWindow(100)
	if !w.take(60) || w.available() != 40 {
		t.Fatalf("take(60): avail = %d, want 40", w.available())
	}
	if w.take(41) {
		t.Fatal("take beyond the window succeeded")
	}
	if err := w.grant(50); err != nil || w.available() != 90 {
		t.Fatalf("grant(50): avail = %d, err = %v", w.available(), err)
	}
	if err := w.grant(11); !errors.Is(err, errWindowExceeded) {
		t.Fatalf("grant past the maximum: err = %v, want errWindowExceeded", err)
	}
	if err := w.grant(0); !errors.Is(err, errWindowInvalid) {
		t.Fatalf("grant(0): err = %v, want errWindowInvalid", err)
	}
}

func TestRecvWindowGrantsAtQuarter(t *testing.T) {
	w := newRecvWindow(100)
	if err := w.onData(90); err != nil {
		t.Fatalf("onData(90): %v", err)
	}
	if grant := w.consume(20); grant != 0 {
		t.Fatalf("grant below a quarter window = %d, want 0", grant)
	}
	if grant := w.consume(5); grant != 25 {
		t.Fatalf("grant at a quarter window = %d, want 25", grant)
	}
	if w.bufferedBytes() != 65 {
		t.Fatalf("buffered = %d, want 65", w.bufferedBytes())
	}
}

func TestRecvWindowRejectsOverrun(t *testing.T) {
	w := newRecvWindow(100)
	if err := w.onData(101); !errors.Is(err, errWindowExceeded) {
		t.Fatalf("onData past the window: err = %v, want errWindowExceeded", err)
	}
}

func TestRecvWindowIgnoresBadConsume(t *testing.T) {
	w := newRecvWindow(100)
	if err := w.onData(10); err != nil {
		t.Fatal(err)
	}
	if grant := w.consume(11); grant != 0 {
		t.Fatalf("consume beyond buffered returned a grant %d", grant)
	}
	if grant := w.consume(-1); grant != 0 {
		t.Fatalf("negative consume returned a grant %d", grant)
	}
}
