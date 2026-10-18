//go:build amd64 && !purego

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

package frame

import "golang.org/x/sys/cpu"

var useAVX2 = cpu.X86.HasAVX2

// maskAVX2 XORs n bytes at b, n a multiple of 32, with key repeated.
//
//go:noescape
func maskAVX2(b *byte, n int, key uint64)

// maskASCIIAVX2 is maskAVX2 that also returns the high bits of every result
// byte ORed together, zero when the unmasked bytes are all ASCII.
//
//go:noescape
func maskASCIIAVX2(b *byte, n int, key uint64) uint32
