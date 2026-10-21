//go:build !windows && !stdio

package uws

// executorRunsTurns reports whether the transport schedules connection turns
// through Options.Executor; the std backend ignores it.
const executorRunsTurns = true
