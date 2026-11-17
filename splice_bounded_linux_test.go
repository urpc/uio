//go:build linux && !stdio

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

package uio

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A bounded SpliceTo whose stream ends before the count fails as io.ReadFull
// does: io.EOF when nothing moved, io.ErrUnexpectedEOF otherwise. The count is
// large enough to take the native splice path.
func TestSpliceToBoundedStreamEndsEarly(t *testing.T) {
	cases := []struct {
		name    string
		send    int
		wantErr error
	}{
		{"nothing", 0, io.EOF},
		{"partial", 100 << 10, io.ErrUnexpectedEOF},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := os.Create(filepath.Join(t.TempDir(), "out"))
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			result := make(chan error, 1)
			ev := &Events{Handler: FuncHandler{Open: func(c Conn) {
				br, err := NewBlockingReader(c)
				if err != nil {
					result <- err
					return
				}
				go func() {
					_, err := br.SpliceTo(context.Background(), out, 1<<20)
					result <- err
					_ = c.Close()
				}()
			}}}
			cli := dialServed(t, ev)
			if tc.send > 0 {
				if _, err := cli.Write(offsetPattern(tc.send)); err != nil {
					t.Fatal(err)
				}
			}
			_ = cli.(*net.TCPConn).CloseWrite()
			select {
			case err := <-result:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("SpliceTo = %v, want %v", err, tc.wantErr)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("SpliceTo did not return at the end of the stream")
			}
		})
	}
}
