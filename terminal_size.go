package portalis

import "fmt"

const (
	defaultTerminalRows = 24
	defaultTerminalCols = 80
	maxTerminalRows     = 65535
	maxTerminalCols     = 65535
	maxTerminalCells    = 1 << 18
)

func validateTerminalSize(rows, cols int) error {
	if rows < 1 || cols < 1 {
		return fmt.Errorf("terminal dimensions must be positive: %dx%d", rows, cols)
	}
	if rows > maxTerminalRows || cols > maxTerminalCols {
		return fmt.Errorf("terminal dimensions exceed %dx%d: %dx%d", maxTerminalRows, maxTerminalCols, rows, cols)
	}
	if rows > maxTerminalCells/cols {
		return fmt.Errorf("terminal area exceeds %d cells: %dx%d", maxTerminalCells, rows, cols)
	}
	return nil
}
