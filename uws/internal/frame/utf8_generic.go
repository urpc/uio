//go:build !amd64 || purego

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

import "unicode/utf8"

const vectorUTF8Min = 32

// ValidUTF8 reports whether p is valid UTF-8, exactly as utf8.Valid does.
func ValidUTF8(p []byte) bool { return utf8.Valid(p) }
