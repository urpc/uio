package bytebuf

import "testing"

func TestBufferPoolSizeClasses(t *testing.T) {
	for _, test := range []struct {
		requested int
		capacity  int
	}{
		{1, 512},
		{16, 512},
		{17, 512},
		{64, 512},
		{65, 512},
		{1024, 1024},
		{65536, 65536},
	} {
		buffer := getBuffer(test.requested)
		if got := buffer.Cap(); got != test.capacity {
			t.Errorf("getBuffer(%d) capacity = %d, want %d", test.requested, got, test.capacity)
		}
		putBuffer(buffer)
	}
}
