//go:build !windows && stdio

package uio

import "github.com/urpc/uio/internal/fdmap"

func stdTestFDLimit() int { return fdmap.MaxOpenFiles }
