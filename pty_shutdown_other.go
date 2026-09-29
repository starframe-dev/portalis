//go:build !linux

package portalis

func isExpectedPTYReadShutdown(error) bool {
	return false
}
