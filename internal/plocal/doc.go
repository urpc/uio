// Package plocal keeps one value per P in front of a sync.Pool: the P-level
// counterpart of thread-local storage, for objects a goroutine borrows and
// returns within a short stretch of work, such as a buffer for one read.
//
// Get takes the value out of the slot of the P the caller runs on and Put
// puts one back; the slot is touched only while the caller is pinned to its
// P, so no other goroutine can reach it meanwhile and the exchange needs no
// atomic operation. That is the whole interface on purpose. Once unpinned, a
// goroutine may move to another P and another goroutine may run on this one,
// so a slot is never lent out in place; and nothing may run while pinned that
// could block or take long, so the package runs no caller code there.
//
// A borrower that returns its value on the P it took it from, which a
// goroutine that does not block in between mostly does, finds the slot full
// again next time and never reaches the pool. Next to sync.Pool this skips the
// lookup of the P's pool shard, the boxing into an interface and the type
// assertion back, and the shared and victim lists on a miss; and a GC, which
// empties a sync.Pool over two cycles, leaves the slots alone. A slot holds
// its value until a Put on its P finds it empty, so a cache retains up to one
// value per P while idle.
//
// The race detector cannot see that pinning orders the accesses to a slot, so
// race-enabled builds use the pool alone.
package plocal
