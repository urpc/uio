package uio

import "testing"

func TestAcquireBuffer(t *testing.T) {
	buffer := AcquireBuffer(8)
	dst := buffer.AvailableBuffer()[:8]
	n := copy(dst, "payload")
	buffer.CommitWrite(n)
	if got := string(buffer.Bytes()); got != "payload" {
		t.Fatalf("buffer = %q, want payload", got)
	}
	ReleaseBuffer(buffer)
}

func TestAcquireBufferRejectsNegativeCapacity(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("negative capacity did not panic")
		}
	}()
	AcquireBuffer(-1)
}
