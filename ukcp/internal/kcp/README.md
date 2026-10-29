# Vendored KCP engine

The files in this directory come from **github.com/xtaci/kcp-go v5.7.1**
(MIT license, `LICENSE` beside this file): the protocol engine alone, without
the `UDPSession`/conn abstraction, goroutines, timers, FEC or encryption the
upstream module keeps in `sess.go`, `fec.go` and friends.

| File | Origin |
|---|---|
| `kcp.go` | upstream, patched (see below) |
| `ringbuffer.go` | upstream, verbatim |
| `bufferpool.go` | upstream, verbatim |
| `snmp.go` | upstream, verbatim |
| `kcp_trace_off.go`, `kcp_trace_on.go` | upstream, verbatim (`-tags debug`) |
| `ukcp_shim.go` | ours: the small additions listed below |

Only stdlib is imported; `go.mod` gains no dependency.

## Patches to `kcp.go`

All three are marked `ukcp:` where they appear.

1. **`Send`**: a zero-length send in stream mode pushes an empty PUSH segment
   instead of returning -1. It takes a sequence number and is acknowledged and
   retransmitted like data, so the end of a stream arrives in order and
   reliably; the peer reads it back as a zero-byte `Recv`.
2. **`currentMs`**: consults `clockNow` (declared in `ukcp_shim.go`) before the
   wall clock, so tests drive the engine's timers instead of waiting on them.
3. **`flush` takes a `FlushType`**: the full flush (`IKCP_FLUSH_FULL`) and the
   read turn's answer-only one (`IKCP_FLUSH_ACKONLY`) share the function, and
   the send queue moves into the send window only on a full flush — an
   answer-only flush must not hand bytes down without sending them, because
   the layer above counts a moved segment as handed to the socket.

## Additions in `ukcp_shim.go`

Accessors upstream keeps on `UDPSession` or does not expose: `SetStreamMode`,
`Flush` (one full flush, returns the next due), `FlushAckOnly`, `AskWindow`,
`IsDead`, `SetDeadLink`, `Queued`, `QueuedBytes`, `Inflight`, `Conv`, `RTT`,
`CWnd`, `RemoteWnd`, and the `mtuLimit` constant upstream keeps in `sess.go`.

## Updating

Fetch the new release, copy the same six files, re-apply the three patches
above and keep `ukcp_shim.go`; check that the fields the shim reads still
exist. The third is structural: an upstream `flush` without the flush type
must be reworked, not just re-copied.
