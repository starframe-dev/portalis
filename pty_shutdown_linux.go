//go:build linux

package portalis

import (
	"errors"
	"syscall"
)

func isExpectedPTYReadShutdown(err error) bool {
	return errors.Is(err, syscall.EIO)
}
