//go:build linux

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

package filerange

import (
	"os"
	"testing"
)

// A regular file of a synthetic file system — sysfs reports a page size that
// is not the file's length — is read to its end rather than sized: a count
// past the reported size does not fail, and a negative count reads to the end.
func TestResolveSyntheticFile(t *testing.T) {
	f, err := os.Open("/sys/kernel/uevent_seqnum")
	if err != nil {
		t.Skipf("no sysfs file available: %v", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		t.Skip("sysfs file is not a sized regular file")
	}
	size := fi.Size()
	r, err := Resolve(f, 0, size+1)
	if err != nil || r.Native || r.Len != size+1 {
		t.Fatalf("Resolve(synthetic, 0, size+1) = %+v, %v; want a non-native read of size+1", r, err)
	}
	r, err = Resolve(f, 0, -1)
	if err != nil || r.Native || r.Len != -1 {
		t.Fatalf("Resolve(synthetic, 0, -1) = %+v, %v; want a non-native read to the end", r, err)
	}
}
