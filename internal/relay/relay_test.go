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

package relay

import "testing"

type fakeSplicer struct {
	can     bool
	enabled *bool
}

func (f fakeSplicer) EnableSplice(enabled bool) bool {
	if !f.can {
		return false
	}
	*f.enabled = enabled
	return true
}

func (fakeSplicer) TrySpliceTo(any, int) (int, bool, error) { return 64, true, nil }

type fakeConn struct {
	Access[fakeGetter]
	splicer *fakeSplicer
}

type fakeGetter struct{}

func (fakeGetter) GetSplicer(source any) Splicer {
	if c, ok := source.(*fakeConn); ok && c.splicer != nil {
		return *c.splicer
	}
	return nil
}

func newFakeConn(can bool) (*fakeConn, *bool) {
	enabled := new(bool)
	return &fakeConn{splicer: &fakeSplicer{can: can, enabled: enabled}}, enabled
}

// A pair relays only when both of its connections can: one that cannot
// switches the other back off.
func TestEnableSplicePairIsBothOrNeither(t *testing.T) {
	a, aOn := newFakeConn(true)
	b, bOn := newFakeConn(true)
	EnableSplicePair(a, b)
	if !*aOn || !*bOn {
		t.Fatalf("pair of relays: enabled %v, %v", *aOn, *bOn)
	}
	c, cOn := newFakeConn(true)
	d, _ := newFakeConn(false)
	EnableSplicePair(c, d)
	if *cOn {
		t.Fatal("a relay paired with one that cannot was left on")
	}
	EnableSplicePair(struct{}{}, a)
	EnableSplicePair(nil, nil)
}

// A source without the capability relays nothing.
func TestTrySpliceNeedsTheCapability(t *testing.T) {
	if n, handled, err := TrySplice(nil, struct{}{}, 10); n != 0 || handled || err != nil {
		t.Fatalf("TrySplice from an unsupported source = (%d, %v, %v)", n, handled, err)
	}
	a, _ := newFakeConn(true)
	if n, handled, err := TrySplice(nil, a, 64); n != 64 || !handled || err != nil {
		t.Fatalf("TrySplice = (%d, %v, %v)", n, handled, err)
	}
}
