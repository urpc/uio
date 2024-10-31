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

package bytebuf

import "io"

type CompositeBuffer struct {
	bufList []*Buffer
}

func NewCompositeBuffer() *CompositeBuffer {
	return &CompositeBuffer{}
}

// Empty reports whether the unread portion of the buffer is empty.
func (b *CompositeBuffer) Empty() bool {
	return len(b.bufList) == 0
}

// Len returns the number of bytes of the unread portion of the buffer;
func (b *CompositeBuffer) Len() (length int) {
	for _, buf := range b.bufList {
		length += buf.Len()
	}
	return
}

// Cap returns the capacity of the buffer's underlying byte slice, that is, the
// total space allocated for the buffer's data.
func (b *CompositeBuffer) Cap() (capacity int) {
	for _, buf := range b.bufList {
		capacity += buf.Cap()
	}
	return
}

// Available returns how many bytes are unused in the buffer.
func (b *CompositeBuffer) Available() (bytes int) {
	for _, buf := range b.bufList {
		bytes += buf.Available()
	}
	return
}

// Reset resets the buffer to be empty,
// but it retains the underlying storage for use by future writes.
func (b *CompositeBuffer) Reset() {
	b.removeRange(len(b.bufList))
}

// Close resets the buffer to be empty, Close is the same as CompositeBuffer.Reset
func (b *CompositeBuffer) Close() error {
	b.Reset()
	return nil
}

// Writev appends the contents of vec to the buffer, growing the buffer as
// needed. The return value n is the length of vec; err is always nil.
func (b *CompositeBuffer) Writev(vec [][]byte) (n int, err error) {
	if 0 == len(vec) {
		return 0, nil
	}

	for _, buf := range vec {
		sent, err := b.Write(buf)
		n += sent
		if err != nil {
			return n, err
		}
	}

	return
}

// Write appends the contents of p to the buffer, growing the buffer as
// needed. The return value n is the length of p; err is always nil.
func (b *CompositeBuffer) Write(p []byte) (n int, err error) {
	if 0 == len(p) {
		return 0, nil
	}

	if sz := len(b.bufList); sz > 0 {
		last := b.bufList[sz-1]
		if space := last.Available(); space > 0 {
			wn := min(space, len(p))
			_, _ = last.Write(p[:wn])
			n += wn
			p = p[wn:]
		}
	}

	if sz := len(p); sz > 0 {
		buffer := getBuffer(sz)
		wn, _ := buffer.Write(p)
		n += wn
		b.bufList = append(b.bufList, buffer)
	}

	return
}

// WriteByte appends the byte c to the buffer, growing the buffer as needed.
// The returned error is always nil, but is included to match [bufio.Writer]'s
// WriteByte.
func (b *CompositeBuffer) WriteByte(c byte) error {
	var data [1]byte
	data[0] = c

	_, err := b.Write(data[:])
	return err
}

// WriteString appends the contents of s to the buffer, growing the buffer as
// needed. The return value n is the length of s; err is always nil.
func (b *CompositeBuffer) WriteString(s string) (n int, err error) {
	if 0 == len(s) {
		return 0, nil
	}

	if sz := len(b.bufList); sz > 0 {
		last := b.bufList[sz-1]
		if space := last.Available(); space > 0 {
			wn := min(space, len(s))
			_, _ = last.WriteString(s[:wn])
			n += wn
			s = s[wn:]
		}
	}

	if sz := len(s); sz > 0 {
		buffer := getBuffer(sz)
		wn, _ := buffer.WriteString(s)
		n += wn
		b.bufList = append(b.bufList, buffer)
	}

	return
}

// ReadFrom reads data from r until EOF and appends it to the buffer, growing
// the buffer as needed. The return value n is the number of bytes read. Any
// error except io.EOF encountered during the read is also returned.
func (b *CompositeBuffer) ReadFrom(r io.Reader) (n int64, err error) {

	if sz := len(b.bufList); sz > 0 {
		last := b.bufList[sz-1]
		if space := last.Available(); space > 0 {
			spaceBuffer := last.AvailableBuffer()
			rn, err := r.Read(spaceBuffer[:space])
			n += int64(rn)
			last.CommitWrite(rn)
			if io.EOF == err {
				return n, nil
			}

			if err != nil {
				return n, err
			}
		}
	}

	buffer := getBuffer(MinRead)
	var rn int64
	if rn, err = buffer.ReadFrom(r); rn > 0 {
		b.bufList = append(b.bufList, buffer)
		n += rn
		return
	}
	putBuffer(buffer)
	return
}

// WriteTo writes data to w until the buffer is drained or an error occurs.
// The return value n is the number of bytes written; it always fits into an
// int, but it is int64 to match the io.WriterTo interface. Any error
// encountered during the write is also returned.
func (b *CompositeBuffer) WriteTo(w io.Writer) (n int64, err error) {
	var endIdx = -1
	for idx, buf := range b.bufList {
		var sz int64
		sz, err = buf.WriteTo(w)
		n += sz
		if nil != err {
			break
		}

		endIdx = idx + 1
	}

	if endIdx > 0 {
		b.removeRange(endIdx)
	}

	return
}

// Read reads the next len(p) bytes from the buffer or until the buffer
// is drained. The return value n is the number of bytes read. If the
// buffer has no data to return, err is io.EOF (unless len(p) is zero);
// otherwise it is nil.
func (b *CompositeBuffer) Read(p []byte) (n int, err error) {
	if 0 == len(b.bufList) {
		return 0, io.EOF
	}

	var endIdx = -1
	for idx, buf := range b.bufList {
		var sz int
		sz, err = buf.Read(p[n:])
		n += sz

		if 0 != buf.Len() || (nil != err && io.EOF != err) {
			endIdx = idx
			break
		}

		endIdx = idx + 1
	}

	if endIdx > 0 {
		b.removeRange(endIdx)
	}

	return
}

// Peek returns the next len(b) bytes without advancing the buffer.
func (b *CompositeBuffer) Peek(p []byte) []byte {
	if 0 == len(b.bufList) || 0 == len(p) {
		return nil
	}

	if buf := b.bufList[0]; buf.Len() >= len(p) {
		return buf.Bytes()[:len(p)]
	}

	var off int
	for _, buf := range b.bufList {
		data := buf.Bytes()
		if off += copy(p[off:], data); off == len(p) {
			break
		}
	}

	return p[:off]
}

// PeekVec returns the [][]bytes without advancing the buffer.
func (b *CompositeBuffer) PeekVec(dst [][]byte) (vec [][]byte, length int) {
	if 0 == len(b.bufList) {
		return
	}

	if nil == dst && len(b.bufList) > 0 {
		dst = make([][]byte, 0, len(b.bufList))
	}

	for _, buf := range b.bufList {
		dst = append(dst, buf.Bytes())
		length += buf.Len()
	}

	return dst, length
}

// Discard advances the inbound buffer with next n bytes, returning the number of bytes discarded.
func (b *CompositeBuffer) Discard(n int) int {
	if 0 == len(b.bufList) {
		return 0
	}

	// number of available bytes
	nBytes := b.Len()

	// discard all unread bytes.
	if n <= 0 {
		b.Reset()
		return nBytes
	}

	if n > nBytes {
		n = nBytes
	}

	var endIdx = -1
	var size = 0

	for idx, buf := range b.bufList {
		sz := buf.Len()
		if sz > n {
			buf.Discard(n)
			endIdx = idx
			size += n
			break
		}

		size += sz
		if n -= sz; 0 == n {
			endIdx = idx + 1
			break
		}
	}

	if endIdx > 0 {
		b.removeRange(endIdx)
	}

	return size
}

func (b *CompositeBuffer) removeRange(endIdx int) {
	if endIdx <= 0 {
		return
	}

	for idx, buf := range b.bufList[:endIdx] {
		putBuffer(buf)
		b.bufList[idx] = nil // avoid memory leak
	}
	if len(b.bufList) == endIdx {
		b.bufList = b.bufList[:0]
	} else {
		b.bufList = b.bufList[endIdx:]
	}
}
