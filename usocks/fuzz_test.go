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

import "testing"

// The handshake parsers face untrusted network input: they must never panic
// and must report a consumed count within the buffer they were given.

func FuzzParseRequest(f *testing.F) {
	f.Add([]byte{Version5, CmdConnect, 0, AtypIPv4, 127, 0, 0, 1, 0x1f, 0x90})
	f.Add([]byte{Version5, CmdConnect, 0, AtypDomain, 3, 'a', 'b', 'c', 0, 80})
	f.Add([]byte{Version5, CmdConnect, 0, AtypIPv6, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 80})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, buf []byte) {
		req, n, ok, err := parseRequest(buf)
		if ok {
			if n < 0 || n > len(buf) {
				t.Fatalf("consumed %d out of range for %d bytes", n, len(buf))
			}
			_ = req.Target()
		}
		if ok && err != nil {
			t.Fatal("ok with an error")
		}
	})
}

func FuzzParseAuth(f *testing.F) {
	f.Add([]byte{authVersion, 1, 'u', 1, 'p'})
	f.Add([]byte{authVersion, 0, 0})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, buf []byte) {
		_, _, n, ok, err := parseAuth(buf)
		if ok && (n < 0 || n > len(buf)) {
			t.Fatalf("consumed %d out of range for %d bytes", n, len(buf))
		}
		if ok && err != nil {
			t.Fatal("ok with an error")
		}
	})
}

func FuzzParseMethodNeg(f *testing.F) {
	f.Add([]byte{Version5, 2, MethodNoAuth, MethodUserPass})
	f.Add([]byte{Version5, 0})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, buf []byte) {
		_, n, ok, err := parseMethodNeg(buf)
		if ok && (n < 0 || n > len(buf)) {
			t.Fatalf("consumed %d out of range for %d bytes", n, len(buf))
		}
		if ok && err != nil {
			t.Fatal("ok with an error")
		}
	})
}
