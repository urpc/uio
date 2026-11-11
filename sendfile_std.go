//go:build windows || stdio

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
	"net"
	"os"

	"github.com/urpc/uio/internal/blockio"
	"github.com/urpc/uio/internal/filerange"
)

func (fc *fdConn) hasNativeSendFile() bool { return false }

func (fc *fdConn) sendFile(file *os.File, offset, count int64) (int64, error) {
	if fc.udp == nil && fc.entry != nil && fc.entry.directConsumer && fc.events().BlockingWrite && !blockio.OnTurn() {
		return fc.ensureBlockWriter().SendFile(file, offset, count)
	}
	return fc.sendFileNonBlocking(file, offset, count)
}

func (fc *fdConn) sendFileNonBlocking(file *os.File, offset, count int64) (int64, error) {
	if fc == nil || fc.udp != nil {
		return 0, ErrUnsupported
	}
	if fc.writeSideClosed() || fc.closingOrDraining() {
		return 0, net.ErrClosed
	}
	return sendFileUserspace(fc, file, offset, count)
}

// sendFileRange reads r into memory: this backend has no sendfile(2), so
// HasNativeSendFile keeps a BlockingWriter from calling it.
func (fc *fdConn) sendFileRange(file *os.File, r filerange.Range, _ uint64) (int64, error) {
	if fc.writeSideClosed() || fc.closingOrDraining() {
		return 0, net.ErrClosed
	}
	return sendFileBuffered(fc, file, r.Start, r.Len)
}

func (fc *fdConn) cancelQueuedFiles(uint64) int64 { return 0 }
