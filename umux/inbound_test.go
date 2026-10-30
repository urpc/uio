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
	"strings"
	"testing"
)

func TestInboundAppendReusesArena(t *testing.T) {
	payload := []byte(strings.Repeat("x", 32))
	var b inbound
	allocs := testing.AllocsPerRun(100, func() {
		b.append(payload)
		b.consume(b.len())
	})
	if allocs != 0 {
		t.Fatalf("append/consume cycle allocated %v times, want 0", allocs)
	}
}

func TestInboundBorrowedRunIsCopiedBeforeAppend(t *testing.T) {
	src := []byte("borrowed")
	var b inbound
	b.setBorrowed(src)
	b.append([]byte("-tail"))
	for i := range src {
		src[i] = 'X'
	}
	if got := string(b.bytes()); got != "borrowed-tail" {
		t.Fatalf("queue = %q, want %q", got, "borrowed-tail")
	}
}

func TestInboundAppendCompactsInPlace(t *testing.T) {
	var b inbound
	b.append(make([]byte, 64))
	b.consume(48)
	base := &b.arena[0]
	b.append(make([]byte, 40))
	if b.off != 0 || b.len() != 56 {
		t.Fatalf("off = %d, len = %d after compaction, want 0 and 56", b.off, b.len())
	}
	if &b.arena[0] != base {
		t.Fatal("compaction reallocated the arena")
	}
}

func TestInboundAppendGrowsBeyondArena(t *testing.T) {
	var b inbound
	b.append(make([]byte, 60))
	b.consume(b.len())
	b.append(make([]byte, 100))
	if cap(b.arena) != 120 || b.len() != 100 {
		t.Fatalf("cap = %d, len = %d, want 120 and 100", cap(b.arena), b.len())
	}
}

func TestInboundDropReleasesArena(t *testing.T) {
	var b inbound
	b.append([]byte("payload"))
	b.drop()
	if b.len() != 0 || b.arena != nil || b.borrowed {
		t.Fatalf("drop left len = %d, arena = %d, borrowed = %v", b.len(), cap(b.arena), b.borrowed)
	}
	b.append([]byte("next"))
	if got := string(b.bytes()); got != "next" {
		t.Fatalf("queue after drop = %q, want %q", got, "next")
	}
}
