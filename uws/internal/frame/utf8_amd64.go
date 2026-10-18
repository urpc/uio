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

import (
	"unicode/utf8"

	segutf8 "github.com/segmentio/asm/utf8"
)

// vectorUTF8Min is where the AVX2 validator starts; below it, it would only
// scan for ASCII before calling utf8.Valid anyway.
const vectorUTF8Min = 32

// ValidUTF8 reports whether p is valid UTF-8, exactly as utf8.Valid does, with
// the AVX2 validator from vectorUTF8Min bytes where the CPU has it.
func ValidUTF8(p []byte) bool {
	if useAVX2 && len(p) >= vectorUTF8Min {
		return segutf8.Valid(p)
	}
	return utf8.Valid(p)
}
