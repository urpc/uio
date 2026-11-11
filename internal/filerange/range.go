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

// Package filerange resolves the part of a file a SendFile sends, for uio and
// for its blocking writer alike.
package filerange

import (
	"io"
	"os"
)

// Range is the part of a file a SendFile sends.
type Range struct {
	// Start is the offset of the range's first byte.
	Start int64
	// Len is how many bytes the range holds, or -1 for a file whose length
	// fstat cannot tell, which is read to its end.
	Len int64
	// Native reports a regular file with a size, which sendfile(2) may serve.
	// A range of such a file runs no further than the size fstat reported.
	Native bool
	// Dev and Ino identify the file of a native range.
	Dev, Ino uint64
	// Reads asks that the range be sent with reads rather than sendfile(2),
	// for a file sendfile(2) refused.
	Reads bool
	// fromPosition reports a range that starts at the file's position, which
	// Finish moves past the bytes sent.
	fromPosition bool
}

// Resolve returns the range SendFile(f, offset, count) sends: from offset, or
// from f's position when offset is negative; count bytes, or through the end
// of the file when count is negative. The end of a file is the size fstat
// reports for a regular file of an ordinary file system; a file that is not
// regular, or one of a synthetic file system (procfs, sysfs and the like),
// whose sizes say nothing of what it holds, is read to its end instead. A
// count that runs past the end of a file fstat sizes fails with
// io.ErrUnexpectedEOF.
func Resolve(f *os.File, offset, count int64) (Range, error) {
	if f == nil {
		return Range{}, os.ErrInvalid
	}
	r := Range{Start: offset, fromPosition: offset < 0}
	if r.fromPosition {
		pos, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return Range{}, err
		}
		r.Start = pos
	}
	// Stat rather than an fstat of Fd: Fd turns a pollable file, a pipe say,
	// back to blocking mode.
	fi, err := f.Stat()
	if err != nil {
		return Range{}, err
	}
	if count == 0 {
		// Nothing to send from anywhere, as an empty transfer always is.
		return r, nil
	}
	size := fi.Size()
	sized := fi.Mode().IsRegular() && size > 0
	if count >= 0 {
		r.Len = count
		// size-r.Start, not r.Start+count: the sum of two values the caller
		// gives may wrap.
		if sized && (r.Start > size || count > size-r.Start) {
			if !synthetic(f) {
				return Range{}, io.ErrUnexpectedEOF
			}
			sized = false
		}
	} else if sized && !synthetic(f) {
		r.Len = max(size-r.Start, 0)
	} else {
		r.Len, sized = -1, false
	}
	if sized {
		r.Native = true
		r.Dev, r.Ino = identity(fi)
	}
	return r, nil
}

// Finish moves the position of a range that started at it past the n bytes
// sent.
func (r Range) Finish(f *os.File, n int64) {
	if r.fromPosition && n > 0 {
		_, _ = f.Seek(r.Start+n, io.SeekStart)
	}
}

// Span is how much of f a reader at its position may send with sendfile(2):
// what fstat says a regular file of an ordinary file system holds past the
// position, within limit when limit is not negative. Zero leaves f to be read
// as any reader is.
func Span(f *os.File, limit int64) (Range, bool) {
	r, err := Resolve(f, -1, -1)
	if err != nil || !r.Native || r.Len <= 0 {
		return Range{}, false
	}
	if limit >= 0 {
		r.Len = min(r.Len, limit)
	}
	return r, r.Len > 0
}
