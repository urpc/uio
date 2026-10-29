//go:build darwin || netbsd || freebsd || openbsd || dragonfly || (linux && (386 || arm || mips || mipsle))

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

package socket

import "syscall"

// SendUDPBatch sends each packet as its own datagram to remote — nil for a
// connected socket — one syscall each: this system has no sendmmsg. Empty
// packets carry no datagram and are skipped.
func SendUDPBatch(fd int, packets [][]byte, remote syscall.Sockaddr) (int, error) {
	sentBytes := 0
	for _, packet := range packets {
		if len(packet) == 0 {
			continue
		}
		sent, err := sendDatagram(fd, packet, remote)
		sentBytes += sent
		if err != nil {
			return sentBytes, err
		}
	}
	return sentBytes, nil
}

// RecvUDPBatch receives as many datagrams as the buffers hold, one syscall
// each: this system has no recvmmsg. Like the batched call, it reports an
// error only when nothing arrived.
func RecvUDPBatch(fd int, packets [][]byte, receives []UDPReceive) (int, error) {
	count := min(len(packets), len(receives))
	got := 0
	for got < count {
		n, err := RecvUDP(fd, packets[got], &receives[got])
		if err != nil {
			if got > 0 {
				return got, nil
			}
			return 0, err
		}
		receives[got].Len = n
		got++
	}
	return got, nil
}

// sendDatagram sends one datagram.
func sendDatagram(fd int, packet []byte, remote syscall.Sockaddr) (int, error) {
	if remote == nil {
		return syscall.Write(fd, packet)
	}
	if err := syscall.Sendto(fd, packet, 0, remote); err != nil {
		return 0, err
	}
	return len(packet), nil
}
