//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly

package socket

// maxWritevBuffers is IOV_MAX on Linux and the BSDs: a writev or sendmsg
// with more buffers fails with EINVAL.
const maxWritevBuffers = 1024

// writevBatches writes a vector longer than writevBatchLimit as one writev
// would, without the limit on how many buffers a call takes. Each Writev
// call takes one batch, and the batches stop at the first one the socket does
// not take in full, so what is left is only what the socket refused. An error
// after some bytes went out is left for the next call: the count returned is
// always what was written, since callers queue the rest, and a would-block
// that hid the count would make them send those bytes again.
func writevBatches(fd int, buffers [][]byte) (int, error) {
	written := 0
	for len(buffers) > 0 {
		batch := buffers[:min(len(buffers), writevBatchLimit)]
		buffers = buffers[len(batch):]
		size := 0
		for _, buffer := range batch {
			size += len(buffer)
		}
		n, err := Writev(fd, batch)
		if err != nil {
			if written > 0 {
				return written, nil
			}
			return n, err
		}
		written += n
		if n < size {
			break
		}
	}
	return written, nil
}
