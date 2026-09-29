package portalis

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/rivo/uniseg"
)

// StyleBits holds text style attributes.
type StyleBits uint8

const (
	StyleBold StyleBits = 1 << iota
	StyleDim
	StyleItalic
	StyleUnderline
	StyleBlink
	StyleReverse
	StyleHidden
	StyleStrikethrough
)

// Cell is a single character cell on the terminal screen.
type Cell struct {
	Rune         rune
	Combining    string
	FG           lipgloss.Color
	BG           lipgloss.Color
	Style        StyleBits
	Continuation bool
}

// Empty returns true if the cell has no visible content.
func (c Cell) Empty() bool {
	return !c.Continuation && (c.Rune == 0 || c.Rune == ' ')
}

type screenRenderState struct {
	lines         [][]Cell
	contentRows   []int
	cols          int
	cursor        Cursor
	cursorVisible bool
}

// Screen is a 2D grid of cells. It is not safe for concurrent use; callers
// that share a Screen must serialize access to its methods and exported fields.
type Screen struct {
	Rows   int
	Cols   int
	Cells  [][]Cell
	Cursor Cursor

	savedCursor             Cursor // DECSC/DECRC state
	savedOriginMode         bool
	savedAutoWrap           bool
	savedWrapPending        bool
	savedCells              [][]Cell
	altSavedCursor          Cursor
	altSavedScrollTop       int
	altSavedScrollBottom    int
	altSavedOriginMode      bool
	altSavedAutoWrap        bool
	altSavedWrapPending     bool
	altScreen               bool
	scrollTop, scrollBottom int  // 0-indexed, DECSTBM. Default 0, Rows-1.
	wrapPending             bool // true after writing to last column; next char wraps.
	originMode              bool
	autoWrap                bool

	scrollback      [][]Cell // lines scrolled off the top
	scrollbackCells int
	scrollbackLimit int
	viewOffset      int // lines scrolled back in the view

	syncActive     bool   // true during synchronized output (ESC[?2026h)
	lastRender     string // last committed render while sync is active
	renderDirty    bool
	committedFrame *screenRenderState

	applicationCursor  bool
	bracketedPaste     bool
	mouseMode1000      bool
	mouseMode1002      bool
	mouseMode1003      bool
	mouseSGR           bool
	focusReporting     bool
	CursorVisible      bool
	CursorBlinkVisible bool // toggled by emulator for blinking cursor

	// selection tracks a text drag-select rectangle. -1 means unset.
	selStartRow, selStartCol int
	selEndRow, selEndCol     int
	selectionActive          bool
}

// StartSelection begins a selection at the given cell.
func (s *Screen) StartSelection(row, col int) {
	if row < 0 || row >= s.Rows || col < 0 || col >= s.Cols {
		return
	}
	s.markDirty()
	row = s.viewportRowToContentRow(row)
	s.selStartRow, s.selStartCol = row, col
	s.selEndRow, s.selEndCol = row, col
	s.selectionActive = true
}

// ExtendSelection updates the selection end to the given cell.
func (s *Screen) ExtendSelection(row, col int) {
	if !s.selectionActive {
		return
	}
	s.markDirty()
	if row < 0 {
		row = 0
	}
	if row >= s.Rows {
		row = s.Rows - 1
	}
	if col < 0 {
		col = 0
	}
	if col >= s.Cols {
		col = s.Cols - 1
	}
	s.selEndRow, s.selEndCol = s.viewportRowToContentRow(row), col
}

// viewportRowToContentRow converts a visible row into the stable logical row
// used by selection and rendering. Live-screen rows are non-negative; rows in
// scrollback are negative, with -1 being the newest scrollback row.
func (s *Screen) viewportRowToContentRow(row int) int {
	return row - s.viewOffset
}

// ClearSelection clears any active selection.
func (s *Screen) ClearSelection() {
	if s.selectionActive {
		s.markDirty()
	}
	s.selectionActive = false
}

// cellInSelection reports whether the given cell is within the current
// selection rectangle. When dragging leftward on a single row, the
// character under the mouse (the cursor, which is the lower col index
// after normalization) is excluded from the selection.
func (s *Screen) cellInSelection(row, col int) bool {
	if !s.selectionActive {
		return false
	}
	aRow, aCol := s.selStartRow, s.selStartCol // anchor (press)
	cRow, cCol := s.selEndRow, s.selEndCol     // cursor (current)

	// When dragging left on the same row, exclude the column under the
	// mouse (which becomes c1 after normalization).
	exclusiveStart := aRow == cRow && aCol > cCol

	r1, c1, r2, c2 := aRow, aCol, cRow, cCol
	if r1 > r2 || (r1 == r2 && c1 > c2) {
		r1, c1, r2, c2 = r2, c2, r1, c1
	}
	if row < r1 || row > r2 {
		return false
	}
	if row == r1 && row == r2 {
		if exclusiveStart {
			return col > c1 && col <= c2
		}
		return col >= c1 && col <= c2
	}
	if row == r1 {
		if exclusiveStart {
			return col > c1
		}
		return col >= c1
	}
	if row == r2 {
		return col <= c2
	}
	return true
}

// SelectionText returns the selected text. It returns nil when the selection
// exceeds clipboard resource limits; trailing whitespace is trimmed.
func (s *Screen) SelectionText() []string {
	lines, err := s.selectionTextWithLimit(maxClipboardBytes, maxClipboardCells)
	if err != nil {
		return nil
	}
	return lines
}

func (s *Screen) selectionTextWithLimit(maxBytes, maxCells int) ([]string, error) {
	if !s.selectionActive {
		return nil, nil
	}
	if maxBytes < 0 || maxCells < 0 {
		return nil, fmt.Errorf("selection limits must be non-negative")
	}
	aRow, aCol := s.selStartRow, s.selStartCol
	cRow, cCol := s.selEndRow, s.selEndCol
	exclusiveStart := aRow == cRow && aCol > cCol

	r1, c1, r2, c2 := aRow, aCol, cRow, cCol
	if r1 > r2 || (r1 == r2 && c1 > c2) {
		r1, c1, r2, c2 = r2, c2, r1, c1
	}
	minRow := -len(s.scrollback)
	if r1 < minRow {
		r1 = minRow
	}
	if r2 >= s.Rows {
		r2 = s.Rows - 1
	}
	if r1 > r2 {
		return nil, nil
	}

	var out []string
	totalBytes, totalCells := 0, 0
	for row := r1; row <= r2; row++ {
		if len(out) > 0 {
			if totalBytes == maxBytes {
				return nil, fmt.Errorf("selection exceeds %d bytes", maxBytes)
			}
			totalBytes++
		}
		start, end := 0, s.Cols-1
		if row == r1 {
			start = c1
			if exclusiveStart {
				start++
			}
		}
		if row == r2 {
			end = c2
		}
		if start > end {
			out = append(out, "")
			continue
		}
		cells := s.contentRowCells(row)
		if start < len(cells) && start > 0 && cells[start].Continuation && cellDisplayWidth(cells[start-1]) == 2 {
			start--
		}
		if start >= len(cells) {
			out = append(out, "")
			continue
		}
		if end >= len(cells) {
			end = len(cells) - 1
		}

		var line strings.Builder
		pendingSpaces := 0
		for col := start; col <= end; col++ {
			if totalCells >= maxCells {
				return nil, fmt.Errorf("selection exceeds %d cells", maxCells)
			}
			totalCells++
			cell := cells[col]
			if cell.Continuation {
				continue
			}
			if cell.Rune == 0 || (cell.Rune == ' ' && cell.Combining == "") {
				pendingSpaces++
				continue
			}
			if pendingSpaces > maxBytes-totalBytes {
				return nil, fmt.Errorf("selection exceeds %d bytes", maxBytes)
			}
			if pendingSpaces > 0 {
				line.WriteString(strings.Repeat(" ", pendingSpaces))
				totalBytes += pendingSpaces
				pendingSpaces = 0
			}
			text := string(cell.Rune) + cell.Combining
			if len(text) > maxBytes-totalBytes {
				return nil, fmt.Errorf("selection exceeds %d bytes", maxBytes)
			}
			line.WriteString(text)
			totalBytes += len(text)
		}
		out = append(out, line.String())
	}
	return out, nil
}

// contentRowCells returns cells for a logical row. Negative rows address the
// scrollback, where -1 is the newest saved line.
func (s *Screen) contentRowCells(row int) []Cell {
	if row < 0 {
		idx := len(s.scrollback) + row
		if idx < 0 || idx >= len(s.scrollback) {
			return nil
		}
		return s.scrollback[idx]
	}
	if row >= len(s.Cells) {
		return nil
	}
	return s.Cells[row]
}

// Cursor represents the terminal cursor position.
type Cursor struct {
	Row   int
	Col   int
	FG    lipgloss.Color
	BG    lipgloss.Color
	Style StyleBits
}

// defaultScrollbackLimit caps the scrollback buffer to prevent unbounded
// memory growth for long-running sessions.
const (
	defaultScrollbackLimit = 10000
	maxScrollbackCells     = 1 << 20
	maxGraphemeBytes       = 4096
)

// NewScreen creates a screen with the requested dimensions. Non-positive
// dimensions are normalized to one cell; dimensions beyond the terminal memory
// budget fall back to the conventional 24x80 size. Scrollback is capped to
// defaultScrollbackLimit lines by default.
func NewScreen(rows, cols int) *Screen {
	if rows < 1 {
		rows = 1
	}
	if cols < 1 {
		cols = 1
	}
	if err := validateTerminalSize(rows, cols); err != nil {
		rows, cols = defaultTerminalRows, defaultTerminalCols
	}
	return newScreen(rows, cols)
}

func newScreen(rows, cols int) *Screen {
	s := &Screen{
		Rows:            rows,
		Cols:            cols,
		scrollBottom:    rows - 1,
		scrollbackLimit: defaultScrollbackLimit,
		CursorVisible:   true,
		autoWrap:        true,
		renderDirty:     true,
	}
	s.resize(rows, cols)
	return s
}

// SetScrollbackLimit sets the maximum number of retained lines. A value of
// zero or less disables this line-count limit; the cell-count safety cap still
// applies.
func (s *Screen) SetScrollbackLimit(limit int) {
	s.markDirty()
	s.scrollbackLimit = limit
	s.enforceScrollbackLimits(maxScrollbackCells)
}

func resizeCellGrid(old [][]Cell, rows, cols int) [][]Cell {
	grid := make([][]Cell, rows)
	for r := 0; r < rows; r++ {
		grid[r] = make([]Cell, cols)
		if r < len(old) {
			copy(grid[r], old[r])
		}
	}
	return grid
}

func resizeCellGridWithTrim(old [][]Cell, rows, cols, cursorRow int) ([][]Cell, int, [][]Cell) {
	if len(old) <= rows {
		return resizeCellGrid(old, rows, cols), 0, nil
	}
	trimTop := len(old) - rows
	if cursorRow < 0 {
		cursorRow = 0
	}
	if cursorRow < trimTop {
		trimTop = cursorRow
	}
	removed := resizeCellGrid(old[:trimTop], trimTop, cols)
	kept := resizeCellGrid(old[trimTop:trimTop+rows], rows, cols)
	return kept, trimTop, removed
}

func (s *Screen) appendResizedScrollback(rows [][]Cell) {
	for _, row := range rows {
		s.appendScrollbackLine(row)
	}
}

func (s *Screen) appendScrollbackLine(cells []Cell) {
	s.appendScrollbackLineWithLimit(cells, maxScrollbackCells)
}

func (s *Screen) appendScrollbackLineWithLimit(cells []Cell, cellLimit int) {
	rowCells := len(cells)
	if rowCells == 0 || cellLimit <= 0 || rowCells > cellLimit {
		return
	}
	maxRows := cellLimit / rowCells
	if s.scrollbackLimit > 0 && s.scrollbackLimit < maxRows {
		maxRows = s.scrollbackLimit
	}
	if s.viewOffset > 0 {
		s.viewOffset++
	}
	var reusable []Cell
	for len(s.scrollback) >= maxRows || s.scrollbackCells > cellLimit-rowCells {
		if len(s.scrollback) == 0 {
			s.scrollbackCells = 0
			break
		}
		oldest := s.scrollback[0]
		s.scrollback = s.scrollback[1:]
		s.scrollbackCells -= len(oldest)
		if s.scrollbackCells < 0 {
			s.scrollbackCells = 0
		}
		if reusable == nil && len(oldest) == rowCells {
			reusable = oldest
		}
	}
	line := reusable
	if line == nil {
		line = make([]Cell, rowCells)
	}
	copy(line, cells)
	s.scrollback = append(s.scrollback, line)
	s.scrollbackCells += rowCells
	if s.viewOffset > len(s.scrollback) {
		s.viewOffset = len(s.scrollback)
	}
}

func (s *Screen) enforceScrollbackLimits(cellLimit int) {
	for len(s.scrollback) > 0 && ((s.scrollbackLimit > 0 && len(s.scrollback) > s.scrollbackLimit) || s.scrollbackCells > cellLimit) {
		s.scrollbackCells -= len(s.scrollback[0])
		s.scrollback = s.scrollback[1:]
	}
	if s.scrollbackCells < 0 {
		s.scrollbackCells = 0
	}
	if s.viewOffset > len(s.scrollback) {
		s.viewOffset = len(s.scrollback)
	}
}

func resizedScrollRegion(top, bottom, oldRows, newRows, trimTop int) (int, int) {
	if top == 0 && bottom == oldRows-1 {
		return 0, newRows - 1
	}
	top -= trimTop
	bottom -= trimTop
	if top < 0 {
		top = 0
	}
	if bottom < 0 {
		bottom = 0
	}
	if top >= newRows {
		top = newRows - 1
	}
	if bottom >= newRows {
		bottom = newRows - 1
	}
	if bottom <= top {
		return 0, newRows - 1
	}
	return top, bottom
}

func clampCursorToSize(cursor Cursor, rows, cols int) Cursor {
	if cursor.Row < 0 {
		cursor.Row = 0
	}
	if cursor.Row >= rows {
		cursor.Row = rows - 1
	}
	if cursor.Col < 0 {
		cursor.Col = 0
	}
	if cursor.Col >= cols {
		cursor.Col = cols - 1
	}
	return cursor
}

func sanitizeCellRow(cells []Cell) {
	for col := range cells {
		cell := cells[col]
		if cell.Continuation {
			if col == 0 || cells[col-1].Continuation || cellDisplayWidth(cells[col-1]) != 2 {
				cells[col] = Cell{}
			}
			continue
		}
		if cell.Rune != 0 && cellDisplayWidth(cell) == 2 {
			if col+1 >= len(cells) || !cells[col+1].Continuation {
				cells[col] = Cell{}
			}
		}
	}
}

func (s *Screen) resize(rows, cols int) {
	s.markDirty()
	oldRows := s.Rows
	activeCells, activeTrim, activeRemoved := resizeCellGridWithTrim(s.Cells, rows, cols, s.Cursor.Row)
	activeTop, activeBottom := resizedScrollRegion(s.scrollTop, s.scrollBottom, oldRows, rows, activeTrim)
	if s.savedCells != nil {
		savedCells, savedTrim, savedRemoved := resizeCellGridWithTrim(s.savedCells, rows, cols, s.altSavedCursor.Row)
		s.savedCells = savedCells
		s.appendResizedScrollback(savedRemoved)
		for row := range s.savedCells {
			sanitizeCellRow(s.savedCells[row])
		}
		s.altSavedScrollTop, s.altSavedScrollBottom = resizedScrollRegion(
			s.altSavedScrollTop, s.altSavedScrollBottom, oldRows, rows, savedTrim,
		)
		s.altSavedCursor.Row -= savedTrim
		s.altSavedCursor = clampCursorToSize(s.altSavedCursor, rows, cols)
	} else {
		s.appendResizedScrollback(activeRemoved)
	}
	s.Cells = activeCells
	s.Cursor.Row -= activeTrim
	s.Rows = rows
	s.Cols = cols
	for row := range s.Cells {
		s.sanitizeWideRow(row)
	}
	// Normalize every scrollback line to the new column count. Lines that
	// were saved at the previous width may be shorter or longer than the
	// new s.Cols; without normalization, older lines keep the stale width
	// and the visible scrollback gets clipped on the right (or padded with
	// garbage when shrunk).
	s.normalizeScrollback()
	s.scrollTop = activeTop
	s.scrollBottom = activeBottom

	// Clamp viewOffset: the scrollback may have shrunk, so the previous
	// offset can now point past the end of the buffer.
	if s.viewOffset > len(s.scrollback) {
		s.viewOffset = len(s.scrollback)
	}

	// Invalidate any cached render so the next frame uses the new dimensions.
	s.lastRender = ""
}

// normalizeScrollback rewrites every line in s.scrollback so that each one
// has exactly s.Cells-equivalent length for the current s.Cols. Lines saved
// at a wider terminal are truncated (with wide-cell continuation cells
// dropped cleanly); lines saved at a narrower terminal are padded with
// blank cells so the renderer doesn't read past the slice.
func (s *Screen) normalizeScrollback() {
	if s.Cols <= 0 {
		return
	}
	s.scrollbackCells = 0
	for i := range s.scrollback {
		line := s.scrollback[i]
		switch {
		case len(line) == s.Cols:
			// Already correct width.
		case len(line) > s.Cols:
			// Truncate. If the cell just past the new boundary is a
			// continuation cell, we are cutting the middle of a wide
			// rune — keep the base wide rune only if it fits cleanly.
			truncated := make([]Cell, s.Cols)
			copy(truncated, line[:s.Cols])
			s.scrollback[i] = truncated
		default:
			// Pad with blank cells.
			padded := make([]Cell, s.Cols)
			copy(padded, line)
			s.scrollback[i] = padded
		}
		// Remove both dangling continuation cells and wide base cells whose
		// continuation was truncated at the new right edge.
		sanitizeCellRow(s.scrollback[i])
		s.scrollbackCells += len(s.scrollback[i])
	}
	s.enforceScrollbackLimits(maxScrollbackCells)
}

// Resize resizes the screen when dimensions are valid. Invalid dimensions
// leave the screen unchanged; use ResizeChecked to retrieve the validation error.
func (s *Screen) Resize(rows, cols int) {
	_ = s.ResizeChecked(rows, cols)
}

// ResizeChecked resizes the screen or returns an error before changing it.
func (s *Screen) ResizeChecked(rows, cols int) error {
	if err := validateTerminalSize(rows, cols); err != nil {
		return err
	}
	if rows == s.Rows && cols == s.Cols {
		return nil
	}
	s.resize(rows, cols)
	s.Cursor = clampCursorToSize(s.Cursor, rows, cols)
	return nil
}

// Clear clears the entire screen.
func (s *Screen) blankCell() Cell {
	return Cell{BG: s.Cursor.BG}
}

func (s *Screen) fillBlank(cells []Cell, start, end int) {
	if start < 0 {
		start = 0
	}
	if end > len(cells) {
		end = len(cells)
	}
	if start >= end {
		return
	}
	blank := s.blankCell()
	for i := start; i < end; i++ {
		cells[i] = blank
	}
}

func (s *Screen) Clear() {
	s.markDirty()
	for r := range s.Cells {
		s.fillBlank(s.Cells[r], 0, len(s.Cells[r]))
	}
	s.wrapPending = false
}

// ClearLine clears the current line from cursor to end.
func (s *Screen) ClearLine() {
	if s.Cursor.Row < 0 || s.Cursor.Row >= s.Rows {
		return
	}
	s.markDirty()
	s.clearCellRange(s.Cursor.Row, s.Cursor.Col, s.Cols)
	s.wrapPending = false
}

// ClearLineLeft clears from start of line to cursor.
func (s *Screen) ClearLineLeft() {
	if s.Cursor.Row < 0 || s.Cursor.Row >= s.Rows {
		return
	}
	s.markDirty()
	s.clearCellRange(s.Cursor.Row, 0, s.Cursor.Col+1)
	s.wrapPending = false
}

// ClearLineAll clears the entire current line.
func (s *Screen) ClearLineAll() {
	if s.Cursor.Row < 0 || s.Cursor.Row >= s.Rows {
		return
	}
	s.markDirty()
	s.fillBlank(s.Cells[s.Cursor.Row], 0, s.Cols)
	s.wrapPending = false
}

func (s *Screen) clearCellRange(row, start, end int) {
	if row < 0 || row >= s.Rows || start >= end {
		return
	}
	if start < 0 {
		start = 0
	}
	if end > s.Cols {
		end = s.Cols
	}
	cells := s.Cells[row]
	if start < len(cells) && cells[start].Continuation && start > 0 {
		start--
	}
	if end < len(cells) && cells[end].Continuation {
		end++
	}
	s.fillBlank(cells, start, end)
}

func (s *Screen) clearCellFootprint(row, col int) {
	if row < 0 || row >= s.Rows || col < 0 || col >= s.Cols {
		return
	}
	cells := s.Cells[row]
	blank := s.blankCell()
	if cells[col].Continuation {
		cells[col] = blank
		if col > 0 {
			cells[col-1] = blank
		}
		return
	}
	if col+1 < len(cells) && cells[col+1].Continuation {
		cells[col+1] = blank
	}
	cells[col] = blank
}

func (s *Screen) sanitizeWideRow(row int) {
	if row < 0 || row >= len(s.Cells) {
		return
	}
	sanitizeCellRow(s.Cells[row])
}

func (s *Screen) previousBaseCell() (row, col int, ok bool) {
	row = s.Cursor.Row
	col = s.Cursor.Col - 1
	if s.wrapPending {
		col = s.Cursor.Col
	}
	if row < 0 || row >= s.Rows || col < 0 || col >= s.Cols {
		return 0, 0, false
	}
	if s.Cells[row][col].Continuation {
		col--
	}
	if col < 0 || s.Cells[row][col].Rune == 0 {
		return 0, 0, false
	}
	return row, col, true
}

func cellText(cell Cell) string {
	if cell.Rune == 0 {
		return ""
	}
	return string(cell.Rune) + cell.Combining
}

func cellDisplayWidth(cell Cell) int {
	if cell.Continuation || cell.Rune == 0 {
		return 0
	}
	return graphemeDisplayWidth(cellText(cell))
}

func graphemeDisplayWidth(text string) int {
	if len(text) > 0 {
		base, _ := utf8.DecodeRuneInString(text)
		isKeycapBase := base >= '0' && base <= '9' || base == '#' || base == '*'
		if isKeycapBase && strings.ContainsRune(text, '\u20e3') {
			return 2
		}
	}
	width := uniseg.StringWidth(text)
	if width > 2 {
		return 2
	}
	return width
}

// isGraphemeExtender reports whether r is a character that extends the previous
// grapheme cluster (combining mark, ZWJ, variation selector, skin tone modifier, etc.).
// This is a fast-path lookup for common cases, avoiding uniseg allocation overhead.
func isGraphemeExtender(r rune) bool {
	switch {
	case r == 0x200D: // ZWJ
		return true
	case r == 0xFE0F || r == 0xFE0E: // Variation selectors (emoji/text presentation)
		return true
	case r >= 0x0300 && r <= 0x036F: // Combining Diacritical Marks
		return true
	case r >= 0x1AB0 && r <= 0x1AFF: // Combining Diacritical Marks Extended
		return true
	case r >= 0x1DC0 && r <= 0x1DFF: // Combining Diacritical Marks Supplement
		return true
	case r >= 0x20D0 && r <= 0x20FF: // Combining Diacritical Marks for Symbols
		return true
	case r >= 0xFE20 && r <= 0xFE2F: // Combining Half Marks
		return true
	case r >= 0x1F3FB && r <= 0x1F3FF: // Skin tone modifiers
		return true
	case r >= 0xE0020 && r <= 0xE007F: // Tag characters (for emoji tag sequences)
		return true
	case r == 0x20E3: // Combining Enclosing Keycap
		return true
	case r >= 0xFE00 && r <= 0xFE0F: // Variation Selectors (full range)
		return true
	default:
		return false
	}
}

// graphemeBreak reports whether there is a grapheme break between prev and r.
// Like ghostty's graphemeBreak: if false, r continues the same cluster as prev.
// Uses a fast-path lookup for common extenders, falling back to uniseg for edge cases.
func graphemeBreak(prev string, r rune) bool {
	if prev == "" {
		return true
	}
	// Fast path: check common grapheme extenders without uniseg allocation.
	if isGraphemeExtender(r) {
		return false
	}
	// Slow path: use uniseg for complex cases (flag sequences, regional indicators, etc.).
	combined := prev + string(r)
	g := uniseg.NewGraphemes(combined)
	if !g.Next() {
		return true
	}
	return g.Str() != combined
}

// graphemeWidthEffect returns the width effect of appending r to prev within a cluster.
// Like ghostty's graphemeWidthEffect: wide, narrow, no_change, ignore.
// Returns (oldWidth, newWidth). If oldWidth == newWidth, effect is no_change.
func graphemeWidthEffect(prev string, r rune) (oldWidth, newWidth int) {
	oldWidth = graphemeDisplayWidth(prev)
	newWidth = graphemeDisplayWidth(prev + string(r))
	return oldWidth, newWidth
}

func (s *Screen) appendToPreviousCluster(r rune) bool {
	row, col, ok := s.previousBaseCell()
	if !ok {
		return false
	}
	cell := &s.Cells[row][col]
	if utf8.RuneLen(r) > 0 && len(cell.Combining)+utf8.RuneLen(r) > maxGraphemeBytes {
		return true
	}
	prev := cellText(*cell)

	// If the previous cell is empty (e.g. continuation cell with Rune==0),
	// there is no cluster to continue — start a new one.
	if prev == "" {
		return false
	}

	// Ghostty: check grapheme break
	if graphemeBreak(prev, r) {
		return false
	}

	// Ghostty: compute width effect
	oldWidth, newWidth := graphemeWidthEffect(prev, r)
	if oldWidth == newWidth {
		// no_change: just append
		cell.Combining += string(r)
		return true
	}

	// Apply width change before appending so the cell state is consistent
	// when we adjust the cursor.
	if oldWidth == 1 && newWidth == 2 {
		if col+1 >= s.Cols {
			if s.Cols < 2 || !s.autoWrap || !s.wrapPending || col != s.Cols-1 || s.Rows <= 1 ||
				(s.Cursor.Row == s.Rows-1 && s.Cursor.Row != s.scrollBottom) ||
				(s.Cursor.Row == s.scrollBottom && s.scrollBottom <= s.scrollTop) {
				return false
			}
			relocated := *cell
			relocated.Combining += string(r)
			s.clearCellFootprint(row, col)
			s.wrapPending = false
			s.Cursor.Col = 0
			s.Index()
			targetRow := s.Cursor.Row
			s.clearCellFootprint(targetRow, 0)
			s.Cells[targetRow][0] = relocated
			s.Cells[targetRow][1] = Cell{Continuation: true}
			if s.Cols == 2 {
				s.Cursor.Col = 1
				s.wrapPending = true
			} else {
				s.Cursor.Col = 2
			}
			s.selectionActive = false
			return true
		}
		s.clearCellFootprint(row, col+1)
		s.Cells[row][col+1] = Cell{Continuation: true}
		if s.Cursor.Row == row && !s.wrapPending {
			if col+2 >= s.Cols {
				s.Cursor.Col = s.Cols - 1
				s.wrapPending = true
			} else {
				s.Cursor.Col = col + 2
			}
		}
	} else if oldWidth == 2 && newWidth == 1 {
		// narrow: clear continuation cell (e.g. VS15 on an emoji)
		if col+1 < s.Cols {
			s.Cells[row][col+1] = Cell{}
		}
		if s.Cursor.Row == row && !s.wrapPending && s.Cursor.Col > col+1 {
			s.Cursor.Col = col + 1
		}
	}

	cell.Combining += string(r)
	return true
}

// Put writes a rune at the cursor position and advances the cursor by its
// terminal cell width. Wide runes occupy a base cell plus a continuation cell;
// combining runes extend the previous grapheme without moving the cursor.
func (s *Screen) Put(r rune) {
	if s.Cursor.Row < 0 || s.Cursor.Row >= s.Rows {
		return
	}
	s.markDirty()

	width := uniseg.StringWidth(string(r))
	if width <= 0 {
		// Extend the previous grapheme before resolving a pending wrap.
		s.appendToPreviousCluster(r)
		return
	}
	if width > 2 {
		width = 2
	}
	if s.autoWrap && s.appendToPreviousCluster(r) {
		return
	}
	if width == 2 && s.Cols < 2 {
		r = '�'
		width = 1
	}

	if s.autoWrap && (s.wrapPending || (width == 2 && s.Cursor.Col == s.Cols-1)) {
		s.wrapPending = false
		s.Cursor.Col = 0
		s.Index()
	} else if !s.autoWrap {
		s.wrapPending = false
		if width == 2 && s.Cursor.Col == s.Cols-1 {
			// A double-width rune cannot fit in the final cell with autowrap
			// disabled. Render a replacement character in-place instead.
			r = '�'
			width = 1
		}
	}

	// After wrap handling, try to append to previous cluster.
	// This is safe because wrapPending is now false and cursor is at col 0
	// (or wherever the wrap left it).
	if s.Cursor.Col > 0 && s.appendToPreviousCluster(r) {
		return
	}

	if s.Cursor.Col < 0 || s.Cursor.Col >= s.Cols {
		return
	}

	row := s.Cursor.Row
	col := s.Cursor.Col
	s.clearCellFootprint(row, col)
	if width == 2 {
		s.clearCellFootprint(row, col+1)
	}
	s.Cells[row][col] = Cell{
		Rune:  r,
		FG:    s.Cursor.FG,
		BG:    s.Cursor.BG,
		Style: s.Cursor.Style,
	}
	if width == 2 {
		s.Cells[row][col+1] = Cell{Continuation: true}
	}

	nextCol := col + width
	if nextCol >= s.Cols {
		s.Cursor.Col = s.Cols - 1
		s.wrapPending = s.autoWrap
		return
	}
	s.Cursor.Col = nextCol
}

// PutBytes bulk-writes printable ASCII bytes to the screen with a single
// markDirty and without allocating a temporary slice. It mirrors Put's
// per-byte semantics (wrap at end of row, cursor advance, wrapPending) but
// does it in one tight pass instead of N function calls.
//
// The caller must guarantee data is all printable ASCII (0x20..0x7e).
// Returns the number of bytes consumed. Callers that discover a non-printable
// byte must fall back to Put for that byte.
func (s *Screen) PutBytes(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	if s.Cursor.Row < 0 || s.Cursor.Row >= s.Rows {
		return 0
	}
	s.markDirty()

	if s.wrapPending && s.autoWrap {
		s.wrapPending = false
		s.Cursor.Col = 0
		s.Index()
		if s.Cursor.Row < 0 || s.Cursor.Row >= s.Rows {
			return 0
		}
	} else if !s.autoWrap {
		s.wrapPending = false
	}

	col := s.Cursor.Col
	if col < 0 || col >= s.Cols {
		return 0
	}

	// Cap the run at the row boundary when autowrap is enabled. With
	// DECAWM disabled, bytes beyond the right edge overwrite the last cell.
	remaining := s.Cols - col
	n := len(data)
	if s.autoWrap && n > remaining {
		n = remaining
	}

	row := s.Cursor.Row
	cells := s.Cells[row]

	// Clear wide-cell footprints intersecting the run boundaries so an ASCII
	// overwrite cannot leave an orphan base or continuation cell.
	if cells[col].Continuation && col > 0 {
		cells[col-1] = s.blankCell()
	}
	if col+n < s.Cols && cells[col+n].Continuation {
		cells[col+n] = s.blankCell()
	}

	// Fill cells directly without allocating a temporary slice.
	fg, bg, style := s.Cursor.FG, s.Cursor.BG, s.Cursor.Style
	if !s.autoWrap {
		for k := 0; k < n; k++ {
			target := col + k
			if target >= s.Cols {
				target = s.Cols - 1
			}
			cells[target] = Cell{Rune: rune(data[k]), FG: fg, BG: bg, Style: style}
		}
		s.Cursor.Col = col + n
		if s.Cursor.Col >= s.Cols {
			s.Cursor.Col = s.Cols - 1
		}
		s.sanitizeWideRow(row)
		return n
	}
	for k := 0; k < n; k++ {
		cells[col+k] = Cell{Rune: rune(data[k]), FG: fg, BG: bg, Style: style}
	}

	if col+n == s.Cols {
		s.wrapPending = true
		// Cursor stays at Cols-1 (last column) until the next write wraps.
		s.Cursor.Col = s.Cols - 1
		return n
	}
	s.Cursor.Col = col + n
	return n
}

// RepeatPrevious repeats the preceding grapheme cluster n times (REP).
func (s *Screen) RepeatPrevious(n int) {
	if n <= 0 {
		n = 1
	}
	row, col, ok := s.previousBaseCell()
	if !ok {
		return
	}
	cell := s.Cells[row][col]
	text := cellText(cell)
	if text == "" {
		return
	}
	for range n {
		for _, r := range text {
			s.Put(r)
		}
	}
}

// SetCursor sets the cursor position (1-indexed in ANSI, 0-indexed here).
func (s *Screen) SetCursor(row, col int) {
	s.markDirty()
	if row < 0 {
		row = 0
	}
	if row >= s.Rows {
		row = s.Rows - 1
	}
	if col < 0 {
		col = 0
	}
	if col >= s.Cols {
		col = s.Cols - 1
	}
	s.Cursor.Row = row
	s.Cursor.Col = col
	s.wrapPending = false
}

// SetCursorAddress applies CUP/HVP/VPA coordinates. In DECOM origin mode the
// row is relative to the active scrolling region.
func (s *Screen) SetCursorAddress(row, col int) {
	if s.originMode {
		row += s.scrollTop
		if row < s.scrollTop {
			row = s.scrollTop
		}
		if row > s.scrollBottom {
			row = s.scrollBottom
		}
	}
	s.SetCursor(row, col)
}

func (s *Screen) SetOriginMode(active bool) {
	s.originMode = active
	if active {
		s.SetCursor(s.scrollTop, 0)
	} else {
		s.SetCursor(0, 0)
	}
}

func (s *Screen) SetAutoWrap(active bool) {
	s.autoWrap = active
	if !active {
		s.wrapPending = false
	}
}

// CursorUp moves the cursor up n rows.
func (s *Screen) CursorUp(n int) {
	row := s.Cursor.Row - n
	if s.originMode && row < s.scrollTop {
		row = s.scrollTop
	}
	s.SetCursor(row, s.Cursor.Col)
}

// CursorDown moves the cursor down n rows.
func (s *Screen) CursorDown(n int) {
	row := s.Cursor.Row + n
	if s.originMode && row > s.scrollBottom {
		row = s.scrollBottom
	}
	s.SetCursor(row, s.Cursor.Col)
}

// CursorForward moves the cursor right n columns.
func (s *Screen) CursorForward(n int) {
	s.SetCursor(s.Cursor.Row, s.Cursor.Col+n)
}

// CursorBackward moves the cursor left n columns.
func (s *Screen) CursorBackward(n int) {
	s.SetCursor(s.Cursor.Row, s.Cursor.Col-n)
}

// CursorNextLine moves the cursor down n rows and to column 0.
func (s *Screen) CursorNextLine(n int) {
	row := s.Cursor.Row + n
	if s.originMode && row > s.scrollBottom {
		row = s.scrollBottom
	}
	s.SetCursor(row, 0)
}

// CursorPrevLine moves the cursor up n rows and to column 0.
func (s *Screen) CursorPrevLine(n int) {
	row := s.Cursor.Row - n
	if s.originMode && row < s.scrollTop {
		row = s.scrollTop
	}
	s.SetCursor(row, 0)
}

// ScrollUp scrolls the active region up by one line.
func (s *Screen) ScrollUp() {
	s.scrollLineUp()
}

// scrollLineUp moves the active region up by one line, saving the scrolled-off
// line to the scrollback buffer if the region starts at the top of the screen.
// A pending selection is cleared because the content the user was selecting
// has moved out of screen coordinates.
func (s *Screen) scrollLineUp() {
	if s.Rows <= 1 {
		return
	}
	s.markDirty()
	top := s.scrollTop
	bottom := s.scrollBottom
	if bottom <= top {
		return
	}
	if s.selectionActive {
		s.selectionActive = false
	}
	if top == 0 && !s.altScreen {
		s.appendScrollbackLine(s.Cells[0])
	}
	for r := top + 1; r <= bottom; r++ {
		copy(s.Cells[r-1], s.Cells[r])
	}
	s.fillBlank(s.Cells[bottom], 0, s.Cols)
}

// Index moves the cursor down, scrolling the active region at its bottom.
func (s *Screen) Index() {
	s.markDirty()
	if s.Cursor.Row == s.scrollBottom {
		s.scrollLineUp()
		return
	}
	if s.Cursor.Row < s.Rows-1 {
		s.Cursor.Row++
	}
}

// NextLine moves to column zero on the next line.
func (s *Screen) NextLine() {
	s.markDirty()
	s.Cursor.Col = 0
	s.wrapPending = false
	s.Index()
}

// ReverseIndex moves the cursor up, scrolling the active region down at its top.
func (s *Screen) ReverseIndex() {
	s.markDirty()
	if s.Cursor.Row == s.scrollTop {
		s.ScrollRegionDown(1)
		return
	}
	if s.Cursor.Row > 0 {
		s.Cursor.Row--
	}
	s.wrapPending = false
}

// ScrollRegionUp scrolls the active region up by n lines.
func (s *Screen) ScrollRegionUp(n int) {
	for range normalizedCount(n, s.scrollBottom-s.scrollTop+1) {
		s.scrollLineUp()
	}
}

// ScrollRegionDown scrolls the active region down by n lines.
func (s *Screen) ScrollRegionDown(n int) {
	s.markDirty()
	n = normalizedCount(n, s.scrollBottom-s.scrollTop+1)
	for r := s.scrollBottom; r >= s.scrollTop+n; r-- {
		copy(s.Cells[r], s.Cells[r-n])
	}
	for r := s.scrollTop; r < s.scrollTop+n; r++ {
		s.fillBlank(s.Cells[r], 0, s.Cols)
	}
	if s.selectionActive {
		s.selectionActive = false
	}
	s.wrapPending = false
}

// InsertChars inserts n blank cells at the cursor.
func (s *Screen) InsertChars(n int) {
	s.markDirty()
	n = normalizedCount(n, s.Cols-s.Cursor.Col)
	row := s.Cells[s.Cursor.Row]
	copy(row[s.Cursor.Col+n:], row[s.Cursor.Col:s.Cols-n])
	s.fillBlank(row, s.Cursor.Col, s.Cursor.Col+n)
	s.sanitizeWideRow(s.Cursor.Row)
	s.wrapPending = false
}

// DeleteChars deletes n cells at the cursor and shifts the remainder left.
func (s *Screen) DeleteChars(n int) {
	s.markDirty()
	n = normalizedCount(n, s.Cols-s.Cursor.Col)
	row := s.Cells[s.Cursor.Row]
	copy(row[s.Cursor.Col:], row[s.Cursor.Col+n:])
	s.fillBlank(row, s.Cols-n, s.Cols)
	s.sanitizeWideRow(s.Cursor.Row)
	s.wrapPending = false
}

// EraseChars clears n cells starting at the cursor without shifting text.
func (s *Screen) EraseChars(n int) {
	s.markDirty()
	n = normalizedCount(n, s.Cols-s.Cursor.Col)
	s.clearCellRange(s.Cursor.Row, s.Cursor.Col, s.Cursor.Col+n)
	s.wrapPending = false
}

// InsertLines inserts n blank lines at the cursor inside the active region.
func (s *Screen) InsertLines(n int) {
	if s.Cursor.Row < s.scrollTop || s.Cursor.Row > s.scrollBottom {
		return
	}
	s.markDirty()
	n = normalizedCount(n, s.scrollBottom-s.Cursor.Row+1)
	for r := s.scrollBottom; r >= s.Cursor.Row+n; r-- {
		copy(s.Cells[r], s.Cells[r-n])
	}
	for r := s.Cursor.Row; r < s.Cursor.Row+n; r++ {
		s.fillBlank(s.Cells[r], 0, s.Cols)
	}
	s.wrapPending = false
}

// DeleteLines deletes n lines at the cursor inside the active region.
func (s *Screen) DeleteLines(n int) {
	if s.Cursor.Row < s.scrollTop || s.Cursor.Row > s.scrollBottom {
		return
	}
	s.markDirty()
	n = normalizedCount(n, s.scrollBottom-s.Cursor.Row+1)
	for r := s.Cursor.Row; r <= s.scrollBottom-n; r++ {
		copy(s.Cells[r], s.Cells[r+n])
	}
	for r := s.scrollBottom - n + 1; r <= s.scrollBottom; r++ {
		s.fillBlank(s.Cells[r], 0, s.Cols)
	}
	s.wrapPending = false
}

func normalizedCount(n, maximum int) int {
	if n <= 0 {
		n = 1
	}
	if n > maximum {
		n = maximum
	}
	return n
}

// SaveCursor saves the current cursor position.
func (s *Screen) SaveCursor() {
	s.savedCursor = s.Cursor
	s.savedOriginMode = s.originMode
	s.savedAutoWrap = s.autoWrap
	s.savedWrapPending = s.wrapPending
}

// RestoreCursor restores the saved cursor position.
func (s *Screen) RestoreCursor() {
	s.markDirty()
	row := s.savedCursor.Row
	col := s.savedCursor.Col
	if row < 0 {
		row = 0
	}
	if row >= s.Rows {
		row = s.Rows - 1
	}
	if col < 0 {
		col = 0
	}
	if col >= s.Cols {
		col = s.Cols - 1
	}
	s.Cursor = s.savedCursor
	s.Cursor.Row = row
	s.Cursor.Col = col
	s.originMode = s.savedOriginMode
	s.autoWrap = s.savedAutoWrap
	s.wrapPending = s.savedWrapPending
}

// EnterAltScreen saves the current screen and clears it.
func (s *Screen) EnterAltScreen() {
	if s.altScreen {
		return
	}
	s.markDirty()
	s.savedCells = resizeCellGrid(s.Cells, s.Rows, s.Cols)
	s.altSavedCursor = s.Cursor
	s.altSavedScrollTop = s.scrollTop
	s.altSavedScrollBottom = s.scrollBottom
	s.altSavedOriginMode = s.originMode
	s.altSavedAutoWrap = s.autoWrap
	s.altSavedWrapPending = s.wrapPending
	s.altScreen = true
	s.viewOffset = 0
	s.selectionActive = false
	s.scrollTop = 0
	s.scrollBottom = s.Rows - 1
	s.originMode = false
	s.Clear()
	s.SetCursor(0, 0)
}

// ExitAltScreen restores the saved screen.
func (s *Screen) ExitAltScreen() {
	if s.savedCells == nil || !s.altScreen {
		return
	}
	s.markDirty()
	s.Cells = s.savedCells
	s.savedCells = nil
	s.altScreen = false
	row := s.altSavedCursor.Row
	col := s.altSavedCursor.Col
	if row < 0 {
		row = 0
	}
	if row >= s.Rows {
		row = s.Rows - 1
	}
	if col < 0 {
		col = 0
	}
	if col >= s.Cols {
		col = s.Cols - 1
	}
	s.Cursor = s.altSavedCursor
	s.Cursor.Row = row
	s.Cursor.Col = col
	s.scrollTop = s.altSavedScrollTop
	s.scrollBottom = s.altSavedScrollBottom
	if s.scrollTop < 0 || s.scrollTop >= s.Rows {
		s.scrollTop = 0
	}
	if s.scrollBottom < s.scrollTop || s.scrollBottom >= s.Rows {
		s.scrollBottom = s.Rows - 1
	}
	s.originMode = s.altSavedOriginMode
	s.autoWrap = s.altSavedAutoWrap
	s.wrapPending = s.altSavedWrapPending
}

// SetScrollRegion sets the scrolling region (DECSTBM).
// top and bottom are 1-indexed (as sent by ANSI).
func (s *Screen) SetScrollRegion(top, bottom int) {
	if top < 1 {
		top = 1
	}
	if bottom > s.Rows {
		bottom = s.Rows
	}
	if bottom <= top {
		bottom = s.Rows
		top = 1
	}
	s.scrollTop = top - 1
	s.scrollBottom = bottom - 1
	if s.originMode {
		s.SetCursor(s.scrollTop, 0)
	} else {
		s.SetCursor(0, 0)
	}
}

// ScrollTop returns the top of the scroll region (0-indexed).
func (s *Screen) ScrollTop() int {
	return s.scrollTop
}

// ScrollBottom returns the bottom of the scroll region (0-indexed).
func (s *Screen) ScrollBottom() int {
	return s.scrollBottom
}

// ClearWrapPending resets the wrap-pending flag.
func (s *Screen) ClearWrapPending() {
	s.wrapPending = false
}

// ViewOffset returns how many lines the view is scrolled back.
func (s *Screen) ViewOffset() int {
	max := len(s.scrollback)
	if s.viewOffset > max {
		return max
	}
	return s.viewOffset
}

// CursorPos returns the current cursor position.
func (s *Screen) CursorPos() (row, col int) {
	return s.Cursor.Row, s.Cursor.Col
}

// LineText returns the plain text of the given screen row, ignoring trailing
// empty cells. It is used to capture the current command line before sending
// Enter to the PTY.
func (s *Screen) LineText(row int) string {
	if row < 0 || row >= s.Rows {
		return ""
	}
	var b strings.Builder
	for _, cell := range s.Cells[row] {
		if cell.Continuation {
			continue
		}
		if cell.Rune == 0 {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(cell.Rune)
		b.WriteString(cell.Combining)
	}
	return strings.TrimRight(b.String(), " ")
}

func (s *Screen) ClearToCursor() {
	s.markDirty()
	for r := 0; r < s.Cursor.Row; r++ {
		s.fillBlank(s.Cells[r], 0, s.Cols)
	}
	s.clearCellRange(s.Cursor.Row, 0, s.Cursor.Col+1)
}

func (s *Screen) ClearScrollback() {
	if len(s.scrollback) == 0 {
		return
	}
	s.markDirty()
	s.scrollback = nil
	s.scrollbackCells = 0
	s.viewOffset = 0
	s.selectionActive = false
}

// ScrollViewUp moves the view up into the scrollback by n lines.
func (s *Screen) ScrollViewUp(n int) {
	if n <= 0 {
		return
	}
	max := len(s.scrollback)
	if max == 0 {
		return
	}
	s.markDirty()
	s.viewOffset += n
	if s.viewOffset > max {
		s.viewOffset = max
	}
}

// ScrollViewDown moves the view down towards the live screen by n lines.
func (s *Screen) ScrollViewDown(n int) {
	if n <= 0 {
		return
	}
	s.markDirty()
	s.viewOffset -= n
	if s.viewOffset < 0 {
		s.viewOffset = 0
	}
}

// ResetView resets the view offset so the live screen is shown.
func (s *Screen) ResetView() {
	if s.viewOffset != 0 {
		s.markDirty()
	}
	s.viewOffset = 0
}

func (s *Screen) mouseTrackingMode() int {
	if s.mouseMode1003 {
		return 1003
	}
	if s.mouseMode1002 {
		return 1002
	}
	if s.mouseMode1000 {
		return 1000
	}
	return 0
}

// SetSync enables or disables synchronized output (ESC[?2026h/l).
// While synchronized output is active, Render() returns the last committed
// frame so intermediate redraw states are not visible.
func (s *Screen) SetSync(active bool) {
	if active {
		if s.syncActive {
			return
		}
		// Freeze the last frame that View actually rendered. Do not materialize
		// a new full-screen string inside Parser.Feed for every sync boundary.
		if s.lastRender == "" {
			s.lastRender = s.renderToString()
			s.renderDirty = false
		}
		if s.committedFrame == nil {
			s.committedFrame = s.captureRenderState()
		}
		s.syncActive = true
		return
	}
	if !s.syncActive {
		return
	}
	// The next View commits the newest logical screen exactly once.
	s.syncActive = false
	s.renderDirty = true
}

func (s *Screen) markDirty() {
	if !s.renderDirty && !s.syncActive && s.lastRender != "" {
		s.committedFrame = s.captureRenderState()
	}
	s.renderDirty = true
}

func (s *Screen) captureRenderState() *screenRenderState {
	viewOffset := s.viewOffset
	if viewOffset > len(s.scrollback) {
		viewOffset = len(s.scrollback)
	}
	frame := &screenRenderState{
		lines:         make([][]Cell, 0, s.Rows),
		contentRows:   make([]int, 0, s.Rows),
		cols:          s.Cols,
		cursor:        s.Cursor,
		cursorVisible: viewOffset == 0 && s.CursorVisible && s.CursorBlinkVisible,
	}
	for viewportRow := 0; viewportRow < s.Rows; viewportRow++ {
		contentRow := viewportRow - viewOffset
		var cells []Cell
		if contentRow < 0 {
			scrollbackRow := len(s.scrollback) + contentRow
			if scrollbackRow >= 0 && scrollbackRow < len(s.scrollback) {
				cells = s.scrollback[scrollbackRow]
			}
		} else if contentRow < len(s.Cells) {
			cells = s.Cells[contentRow]
		}
		frame.lines = append(frame.lines, append([]Cell(nil), cells...))
		frame.contentRows = append(frame.contentRows, contentRow)
	}
	return frame
}

func (s *Screen) renderCommittedWithSelection() string {
	frame := s.committedFrame
	if frame == nil {
		return s.lastRender
	}
	liveCursor, liveCols := s.Cursor, s.Cols
	defer func() {
		s.Cursor = liveCursor
		s.Cols = liveCols
	}()
	s.Cursor = frame.cursor
	s.Cols = frame.cols
	lines := make([]string, len(frame.lines))
	for viewportRow, cells := range frame.lines {
		contentRow := frame.contentRows[viewportRow]
		cursorRow := frame.cursorVisible && contentRow == frame.cursor.Row
		lines[viewportRow] = s.renderCells(cells, cursorRow, contentRow)
	}
	return strings.Join(lines, "\n")
}

// Render returns the screen as a string. If the view is scrolled back,
// lines are taken from the scrollback buffer; otherwise the live screen is
// rendered.
func (s *Screen) Render() string {
	if s.syncActive {
		if !s.selectionActive {
			return s.lastRender
		}
		return s.renderCommittedWithSelection()
	}
	if !s.renderDirty && s.lastRender != "" && !s.selectionActive {
		return s.lastRender
	}
	s.lastRender = s.renderToString()
	s.committedFrame = nil
	s.renderDirty = false
	return s.lastRender
}

// renderToString renders the current screen state without considering sync.
func (s *Screen) renderToString() string {
	var lines []string
	cursorHere := s.viewOffset == 0 && s.CursorVisible && s.CursorBlinkVisible
	if s.viewOffset > len(s.scrollback) {
		s.viewOffset = len(s.scrollback)
	}
	for viewportRow := 0; viewportRow < s.Rows; viewportRow++ {
		contentRow := s.viewportRowToContentRow(viewportRow)
		if contentRow < 0 {
			sbIdx := len(s.scrollback) + contentRow
			lines = append(lines, s.renderScrollbackLine(sbIdx, contentRow))
			continue
		}
		lines = append(lines, s.renderLine(contentRow, cursorHere && contentRow == s.Cursor.Row))
	}
	return strings.Join(lines, "\n")
}

func (s *Screen) renderLine(r int, cursorRow bool) string {
	if r < 0 || r >= len(s.Cells) {
		return strings.Repeat(" ", s.Cols)
	}
	return s.renderCells(s.Cells[r], cursorRow, r)
}

func (s *Screen) renderScrollbackLine(idx, contentRow int) string {
	if idx < 0 || idx >= len(s.scrollback) {
		return strings.Repeat(" ", s.Cols)
	}
	return s.renderCells(s.scrollback[idx], false, contentRow)
}

func (s *Screen) renderCells(cells []Cell, cursorRow bool, row int) string {
	var b strings.Builder
	var lastFG, lastBG lipgloss.Color
	var lastStyle StyleBits

	for c := 0; c < s.Cols; c++ {
		cell := Cell{Rune: ' '}
		if c < len(cells) {
			cell = cells[c]
		}
		if cell.Continuation {
			continue
		}
		if cell.Rune == 0 {
			cell.Rune = ' '
		}
		text := string(cell.Rune) + cell.Combining

		cursorHere := cursorRow && (c == s.Cursor.Col ||
			(cellDisplayWidth(cell) == 2 && s.Cursor.Col == c+1))
		selHere := s.cellInSelection(row, c) ||
			(c+1 < len(cells) && cells[c+1].Continuation && s.cellInSelection(row, c+1))

		if cursorHere || selHere {
			// Close current style before applying reverse video (cursor
			// or selection highlight).
			if lastFG != "" || lastBG != "" || lastStyle != 0 {
				b.WriteString("\x1b[0m")
			}
			b.WriteString("\x1b[7m")
			overlayText := text
			if cell.Style&StyleHidden != 0 {
				width := cellDisplayWidth(cell)
				if width < 1 {
					width = 1
				}
				overlayText = strings.Repeat(" ", width)
			}
			b.WriteString(overlayText)
			b.WriteString("\x1b[0m")
			// Re-emit the cell's style for subsequent cells.
			if cell.FG != "" || cell.BG != "" || cell.Style != 0 {
				b.WriteString(renderStyle(cell.FG, cell.BG, cell.Style))
				lastFG = cell.FG
				lastBG = cell.BG
				lastStyle = cell.Style
			} else {
				lastFG = ""
				lastBG = ""
				lastStyle = 0
			}
			continue
		}

		// Emit style changes only when needed
		if cell.FG != lastFG || cell.BG != lastBG || cell.Style != lastStyle {
			b.WriteString(renderStyle(cell.FG, cell.BG, cell.Style))
			lastFG = cell.FG
			lastBG = cell.BG
			lastStyle = cell.Style
		}
		b.WriteString(text)
	}
	// Reset style at end of line
	if lastFG != "" || lastBG != "" || lastStyle != 0 {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

func renderStyle(fg, bg lipgloss.Color, style StyleBits) string {
	var parts []string
	parts = append(parts, "0") // reset first
	if style&StyleBold != 0 {
		parts = append(parts, "1")
	}
	if style&StyleDim != 0 {
		parts = append(parts, "2")
	}
	if style&StyleItalic != 0 {
		parts = append(parts, "3")
	}
	if style&StyleUnderline != 0 {
		parts = append(parts, "4")
	}
	if style&StyleBlink != 0 {
		parts = append(parts, "5")
	}
	if style&StyleReverse != 0 {
		parts = append(parts, "7")
	}
	if style&StyleHidden != 0 {
		parts = append(parts, "8")
	}
	if style&StyleStrikethrough != 0 {
		parts = append(parts, "9")
	}
	if fg != "" {
		parts = append(parts, sgrColor(fg, false))
	}
	if bg != "" {
		parts = append(parts, sgrColor(bg, true))
	}
	return "\x1b[" + strings.Join(parts, ";") + "m"
}

func sgrColor(c lipgloss.Color, bg bool) string {
	s := string(c)
	if strings.HasPrefix(s, "#") && len(s) == 7 {
		base := 38
		if bg {
			base = 48
		}
		r, _ := strconv.ParseInt(s[1:3], 16, 0)
		g, _ := strconv.ParseInt(s[3:5], 16, 0)
		b, _ := strconv.ParseInt(s[5:7], 16, 0)
		return fmt.Sprintf("%d;2;%d;%d;%d", base, r, g, b)
	}
	return ""
}
