// Package fdmap maps OS descriptors to connection objects using the cheapest
// synchronization available on each platform.
package fdmap

import "errors"

var ErrOutOfRange = errors.New("fdmap: descriptor out of range")
