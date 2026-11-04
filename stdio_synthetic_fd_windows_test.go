//go:build windows

package uio

// syntheticFD keeps the hint: the Windows fd table has no range limit.
func syntheticFD(hint uintptr) uintptr { return hint }
