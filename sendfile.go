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
	"os"

	"github.com/urpc/uio/internal/bytebuf"
	"github.com/urpc/uio/internal/filerange"
)

const (
	sendfileReadChunk = 32 << 10
	// sendfileReadMax is the most a SendFile that cannot use sendfile(2)
	// reads into memory, on a connection without MaxOutboundBuffered.
	sendfileReadMax = 4 << 20
)

// SendFile transmits count bytes of file starting at offset over conn without
// mutating file's seek position when offset >= 0. If offset < 0, SendFile
// starts at file's current seek position and advances it past what it sends.
// If count < 0, bytes from the starting offset through the end of file are
// sent. A count that runs past the end of the file fails with
// io.ErrUnexpectedEOF, before anything is sent when fstat knows the file's
// size.
//
// file must support reading at an offset (pread); pipes and sockets do not.
// The end of file is the size fstat reports for a regular file of an ordinary
// file system. Files of synthetic file systems (procfs, sysfs and the like),
// whose sizes say nothing of what they hold, are read to their end instead.
//
// On a native stream connection SendFile sends with sendfile(2), behind the
// bytes written before it and ahead of those written after it. Outside the
// connection's turns, a range that finds nothing queued goes straight to the
// socket, and what the socket does not take is queued; inside a turn, the
// range queues behind the turn's replies and leaves with them. A queued range
// holds a duplicate of file's descriptor, not memory — one of 16 KiB or less
// is read into memory instead — so the caller may close file as soon as
// SendFile returns. It is admitted whole, however long, while the queue is
// under MaxOutboundBuffered, and counts toward the limit until sent: writes
// that follow it are refused with ErrOutboundOverflow, or wait in a
// BlockingWriter, while the queue is full. A file sendfile(2) cannot serve is
// read instead.
//
// When Events.BlockingWrite is enabled and SendFile is called outside an
// event-loop turn, it delegates to [BlockingWriter.SendFile], which streams
// any file with backpressure.
//
// Otherwise — on layered and portable stdio connections, on platforms
// without sendfile(2), and for files fstat does not size — SendFile reads the
// range into pooled buffers and queues them before it returns. It reads no
// more than the queue has room for under MaxOutboundBuffered, or 4 MiB
// without a limit, and refuses a longer range with ErrOutboundOverflow before
// queueing anything; a BlockingWriter streams those.
func SendFile(conn Lower, file *os.File, offset, count int64) (int64, error) {
	if conn == nil {
		return 0, ErrUnsupported
	}
	if sl, ok := conn.(socketLower); ok {
		conn = sl.Conn
	}
	if fdc, ok := conn.(*fdConn); ok {
		return fdc.sendFile(file, offset, count)
	}
	if ln, ok := conn.(*layerNode); ok && ln.isTop() && ln.stack.blockingWrite && !OnTurn() {
		return ln.ensureBlockWriter().SendFile(file, offset, count)
	}
	return sendFileUserspace(conn, file, offset, count)
}

func sendFileUserspace(dst Lower, file *os.File, offset, count int64) (int64, error) {
	r, err := filerange.Resolve(file, offset, count)
	if err != nil || r.Len == 0 {
		return 0, err
	}
	n, err := sendFileBuffered(dst, file, r.Start, r.Len)
	r.Finish(file, n)
	return n, err
}

func writeOwnedNonBlockingForSendFile(dst Lower, buf *Buffer) (int, error) {
	if fdc, ok := dst.(*fdConn); ok {
		return fdc.writeOwnedNonBlocking(buf)
	}
	if ln, ok := dst.(*layerNode); ok {
		return ln.writeOwnedPreservePartial(buf)
	}
	return dst.WriteOwned(buf)
}

// sendFileBudget is how much a SendFile on dst may read into memory: the
// room its queue has under MaxOutboundBuffered, or sendfileReadMax.
func sendFileBudget(dst Lower) int64 {
	limit := 0
	switch c := dst.(type) {
	case *fdConn:
		limit = c.payloadLimit()
	case *layerNode:
		limit = c.stack.limit
	}
	if limit <= 0 {
		return sendfileReadMax
	}
	return int64(limit - dst.OutboundBuffered())
}

func sendFileOverflow(dst Lower) error {
	if fdc, ok := dst.(*fdConn); ok {
		return fdc.errOutboundOverflow()
	}
	if ln, ok := dst.(*layerNode); ok {
		if fdc, ok := ln.stack.socket.(*fdConn); ok {
			return fdc.errOutboundOverflow()
		}
	}
	return ErrOutboundOverflow
}

// sendFileBuffered queues count bytes of file from offset, or all it holds
// there when count is negative, read into pooled buffers before it returns.
// It reads the range whole before queueing any of it, and no more than
// sendFileBudget allows: a longer range is refused with ErrOutboundOverflow,
// and a file that ends before count bytes with io.ErrUnexpectedEOF, with
// nothing queued. Another writer filling the queue while it queues the
// buffers cuts it short, as it would a Write: the count is what it queued.
func sendFileBuffered(dst Lower, file *os.File, offset, count int64) (int64, error) {
	budget := sendFileBudget(dst)
	if count > budget {
		return 0, sendFileOverflow(dst)
	}
	var bufs []*bytebuf.Buffer
	defer func() {
		for _, buf := range bufs {
			bytebuf.ReleaseBuffer(buf)
		}
	}()
	var size int64
	for count < 0 || size < count {
		chunk := int64(sendfileReadChunk)
		if count >= 0 {
			chunk = min(chunk, count-size)
		}
		buf := bytebuf.AcquireBuffer(int(chunk))
		nr, err := file.ReadAt(buf.AvailableBuffer()[:chunk], offset+size)
		if nr > 0 {
			buf.CommitWrite(nr)
			bufs = append(bufs, buf)
			size += int64(nr)
		} else {
			bytebuf.ReleaseBuffer(buf)
		}
		if size > budget {
			return 0, sendFileOverflow(dst)
		}
		if err == io.EOF || (err == nil && nr == 0) {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if count >= 0 && size < count {
		return 0, io.ErrUnexpectedEOF
	}
	var sent int64
	for len(bufs) > 0 {
		buf := bufs[0]
		bufs = bufs[1:]
		nw, err := writeOwnedNonBlockingForSendFile(dst, buf)
		if errors.Is(err, ErrOutboundOverflow) {
			bytebuf.ReleaseBuffer(buf)
		}
		sent += int64(nw)
		if err != nil {
			return sent, err
		}
	}
	return sent, nil
}

// SendFile transmits count bytes of file starting at offset over w, suspending
// while the outbound window is full. If offset < 0, SendFile starts at file's
// current seek position. If count < 0, bytes through the end of file are sent.
func (w *BlockingWriter) SendFile(file *os.File, offset, count int64) (int64, error) {
	return w.w.SendFile(file, offset, count)
}

// SendFileContext transmits count bytes of file starting at offset over w,
// honoring ctx cancellation and write deadlines while suspended.
func (w *BlockingWriter) SendFileContext(ctx context.Context, file *os.File, offset, count int64) (int64, error) {
	return w.w.SendFileContext(ctx, file, offset, count)
}

// SendFile transmits count bytes of file starting at offset over bc's writer.
func (bc *BlockingConn) SendFile(file *os.File, offset, count int64) (int64, error) {
	return bc.w.SendFile(file, offset, count)
}

// SendFileContext transmits count bytes of file starting at offset over bc's
// writer, honoring ctx cancellation and write deadlines while suspended.
func (bc *BlockingConn) SendFileContext(ctx context.Context, file *os.File, offset, count int64) (int64, error) {
	return bc.w.SendFileContext(ctx, file, offset, count)
}
