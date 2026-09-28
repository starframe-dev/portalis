package portalis

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

// Parser parses ANSI escape sequences and updates a Screen.
type Parser struct {
	screen             *Screen
	state              ansiState
	buf                strings.Builder
	utf8Buf            []byte
	onCWD              func(string)
	onResponse         func([]byte)
	lastCWD            string
	escapeIntermediate byte
	g0LineDrawing      bool
	g1LineDrawing      bool
	useG1              bool
	savedG0LineDrawing bool
	savedG1LineDrawing bool
	savedUseG1         bool
}

type ansiState int

const (
	maxCSISequenceBytes = 4096
	maxOSCSequenceBytes = 64 * 1024
)

const (
	stateNormal ansiState = iota
	stateEscape
	stateEscapeIntermediate
	stateCSI
	stateOSC
	stateString
	stateStringEscape
)

// NewParser creates a new ANSI parser for the given screen.
func NewParser(screen *Screen) *Parser {
	return &Parser{screen: screen}
}

// SetCWDCallback sets the callback invoked when the working directory changes.
func (p *Parser) SetCWDCallback(fn func(string)) {
	p.onCWD = fn
}

// SetResponseCallback receives terminal-generated replies such as DSR/DA.
// The callback must not block; Emulator queues replies and writes them after
// releasing its state mutex.
func (p *Parser) SetResponseCallback(fn func([]byte)) {
	p.onResponse = fn
}

func (p *Parser) respond(data []byte) {
	if p.onResponse == nil || len(data) == 0 {
		return
	}
	p.onResponse(append([]byte(nil), data...))
}

func (p *Parser) saveCursorState() {
	p.screen.SaveCursor()
	p.savedG0LineDrawing = p.g0LineDrawing
	p.savedG1LineDrawing = p.g1LineDrawing
	p.savedUseG1 = p.useG1
}

func (p *Parser) restoreCursorState() {
	p.screen.RestoreCursor()
	p.g0LineDrawing = p.savedG0LineDrawing
	p.g1LineDrawing = p.savedG1LineDrawing
	p.useG1 = p.savedUseG1
}

// flushUtf8 flushes any incomplete UTF-8 sequence as replacement chars.
func (p *Parser) flushUtf8() {
	if len(p.utf8Buf) == 0 {
		return
	}
	// Incomplete sequence — emit one replacement rune for the invalid scalar.
	p.screen.Put('\ufffd')
	p.utf8Buf = p.utf8Buf[:0]
}

// utf8Valid returns true if the byte slice is a complete, valid UTF-8 sequence.
func utf8Valid(buf []byte) bool {
	return utf8.FullRune(buf) && utf8.Valid(buf)
}

// Feed feeds data into the parser.
func (p *Parser) Feed(data []byte) {
	if len(data) > 0 {
		defer p.screen.markDirty()
	}
	i := 0
	for i < len(data) {
		b := data[i]
		// Fast path: long runs of printable ASCII in stateNormal with no
		// active line-drawing set or pending UTF-8 sequence. The common
		// case for modern shells/programs is straight ASCII text, and
		// going through feedByte's switch per byte is the main cost.
		if b >= 0x20 && b <= 0x7e &&
			p.state == stateNormal &&
			len(p.utf8Buf) == 0 &&
			!p.useG1 && !p.g0LineDrawing && !p.g1LineDrawing {
			// Find the run length.
			j := i + 1
			for j < len(data) && data[j] >= 0x20 && data[j] <= 0x7e {
				j++
			}
			// Bulk-write the ASCII run via PutBytes. If the run spans the
			// end of the row, PutBytes consumes up to the row boundary
			// and signals wrapPending; the remaining bytes are written on
			// the next iteration (which will see wrapPending and wrap).
			for k := i; k < j; {
				written := p.screen.PutBytes(data[k:j])
				if written == 0 {
					break
				}
				k += written
			}
			i = j
			continue
		}
		p.feedByte(b)
		i++
	}
}

func (p *Parser) feedByte(b byte) {
	switch p.state {
	case stateNormal:
		// An ASCII/control byte cannot continue a pending UTF-8 sequence.
		// Flush the incomplete bytes before processing the current byte.
		if b < 0x80 && len(p.utf8Buf) > 0 {
			p.flushUtf8()
		}
		if b == 0x1b {
			p.flushUtf8()
			p.state = stateEscape
			p.buf.Reset()
			return
		}
		if b == '\r' {
			p.flushUtf8()
			p.screen.Cursor.Col = 0
			p.screen.wrapPending = false
			return
		}
		if b == '\n' {
			p.flushUtf8()
			// If a wrap is pending, LF behaves like CR+LF.
			if p.screen.wrapPending {
				p.screen.wrapPending = false
				p.screen.Cursor.Col = 0
			}
			p.screen.Index()
			return
		}
		if b == '\t' {
			p.flushUtf8()
			p.screen.wrapPending = false
			next := (p.screen.Cursor.Col/8 + 1) * 8
			if next >= p.screen.Cols {
				next = p.screen.Cols - 1
			}
			p.screen.Cursor.Col = next
			return
		}
		if b == '\b' {
			p.flushUtf8()
			p.screen.wrapPending = false
			if p.screen.Cursor.Col > 0 {
				p.screen.Cursor.Col--
			}
			return
		}
		if b == 0x0e {
			p.flushUtf8()
			p.useG1 = true
			return
		}
		if b == 0x0f {
			p.flushUtf8()
			p.useG1 = false
			return
		}

		// Accept 8-bit C1 forms when they are not part of a pending UTF-8
		// sequence. Modern programs usually use 7-bit ESC forms, but xterm
		// compatibility permits both.
		if len(p.utf8Buf) == 0 {
			switch b {
			case 0x90, 0x98, 0x9e, 0x9f: // DCS, SOS, PM, APC
				p.state = stateString
				return
			case 0x9b: // CSI
				p.state = stateCSI
				p.buf.Reset()
				return
			case 0x9d: // OSC
				p.state = stateOSC
				p.buf.Reset()
				return
			}
		}

		// Collect UTF-8 multi-byte sequences.
		if b >= 0x80 {
			p.utf8Buf = append(p.utf8Buf, b)
			if utf8.FullRune(p.utf8Buf) {
				if utf8.Valid(p.utf8Buf) {
					r, _ := utf8.DecodeRune(p.utf8Buf)
					p.screen.Put(r)
				} else {
					p.screen.Put('\ufffd')
				}
				p.utf8Buf = p.utf8Buf[:0]
			}
			return
		}

		// ASCII printable characters.
		if b >= 0x20 && b != 0x7f {
			r := rune(b)
			lineDrawing := p.g0LineDrawing
			if p.useG1 {
				lineDrawing = p.g1LineDrawing
			}
			if lineDrawing {
				r = decSpecialGraphic(b)
			}
			p.screen.Put(r)
		}

	case stateEscape:
		if b == '[' {
			p.state = stateCSI
			p.buf.Reset()
			return
		}
		if b == ']' {
			p.state = stateOSC
			p.buf.Reset()
			return
		}
		switch b {
		case '7':
			p.saveCursorState()
		case '8':
			p.restoreCursorState()
		case 'P', 'X', '^', '_': // DCS, SOS, PM, APC: ignore until ST
			p.state = stateString
			return
		case 'D':
			p.screen.Index()
		case 'E':
			p.screen.NextLine()
		case 'M':
			p.screen.ReverseIndex()
		default:
			if b >= 0x20 && b <= 0x2f {
				p.escapeIntermediate = b
				p.state = stateEscapeIntermediate
				return
			}
		}
		p.state = stateNormal

	case stateEscapeIntermediate:
		if b >= 0x30 && b <= 0x7e {
			p.handleEscapeIntermediate(p.escapeIntermediate, b)
			p.state = stateNormal
			p.escapeIntermediate = 0
		}

	case stateCSI:
		// Bound malformed/unterminated control sequences so child output cannot
		// grow parser memory without limit.
		if p.buf.Len() >= maxCSISequenceBytes {
			p.buf.Reset()
			p.state = stateNormal
			return
		}
		// Collect until final byte (0x40-0x7e)
		p.buf.WriteByte(b)
		if b >= 0x40 && b <= 0x7e {
			p.handleCSI(p.buf.String())
			p.state = stateNormal
			p.buf.Reset()
		}

	case stateOSC:
		if p.buf.Len() >= maxOSCSequenceBytes {
			p.buf.Reset()
			p.state = stateNormal
			return
		}
		if b == '\x07' || b == 0x9c {
			p.handleOSC(p.buf.String())
			p.state = stateNormal
			p.buf.Reset()
			return
		}
		if b == '\x1b' {
			// Expect ST (ESC \).
			p.buf.WriteByte(b)
			return
		}
		if b == '\\' && p.buf.Len() > 0 && p.buf.String()[p.buf.Len()-1] == 0x1b {
			payload := p.buf.String()
			if len(payload) > 0 {
				payload = payload[:len(payload)-1]
			}
			p.handleOSC(payload)
			p.state = stateNormal
			p.buf.Reset()
			return
		}
		p.buf.WriteByte(b)

	case stateString:
		if b == 0x9c {
			p.state = stateNormal
			return
		}
		if b == '\x1b' {
			p.state = stateStringEscape
		}
	case stateStringEscape:
		if b == '\\' {
			p.state = stateNormal
		} else if b != '\x1b' {
			p.state = stateString
		}
	}
}

func (p *Parser) handleOSC(payload string) {
	if len(payload) < 2 {
		return
	}
	parts := strings.SplitN(payload, ";", 2)
	if len(parts) < 2 {
		return
	}
	if parts[0] != "7" {
		return
	}
	path := extractOSC7Path(parts[1])
	if path != "" && path != p.lastCWD {
		p.lastCWD = path
		if p.onCWD != nil {
			p.onCWD(path)
		}
	}
}

// extractOSC7Path extracts an absolute filesystem path from an OSC 7 payload.
// Supported forms:
//
//	file://hostname/path  → /path
//	/absolute/path        → /absolute/path
func extractOSC7Path(s string) string {
	s = strings.TrimSpace(s)
	var path string
	if strings.HasPrefix(s, "file://") {
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "file" {
			return ""
		}
		path = u.Path
	} else if filepath.IsAbs(s) {
		path = s
	} else {
		return ""
	}
	for _, r := range path {
		if r == 0 || r == '\x1b' || r == '\x07' || r == '\r' || r == '\n' {
			return ""
		}
	}
	return path
}

func (p *Parser) handleCSI(seq string) {
	if len(seq) == 0 {
		return
	}
	final := seq[len(seq)-1]
	rawParams := seq[:len(seq)-1]

	// Detect private marker (? for DEC private, >/< for other private forms).
	isPrivate := false
	var privateMarker byte
	if len(rawParams) > 0 {
		switch rawParams[0] {
		case '?', '>', '<', '=', '!':
			isPrivate = true
			privateMarker = rawParams[0]
			rawParams = rawParams[1:]
		}
	}
	params := parseParams(rawParams)

	switch final {
	case 'm':
		if isPrivate {
			break // ignore private SGR
		}
		p.handleSGR(params)
	case 'H', 'f':
		row := 1
		col := 1
		if len(params) > 0 {
			row = params[0]
		}
		if len(params) > 1 {
			col = params[1]
		}
		p.screen.SetCursorAddress(row-1, col-1)
	case 'J':
		n := 0
		if len(params) > 0 {
			n = params[0]
		}
		switch n {
		case 0:
			p.clearFromCursor()
		case 1:
			p.screen.ClearToCursor()
		case 2:
			p.screen.Clear()
		case 3:
			p.screen.ClearScrollback()
		}
	case 'K':
		n := 0
		if len(params) > 0 {
			n = params[0]
		}
		switch n {
		case 0:
			p.screen.ClearLine()
		case 1:
			p.screen.ClearLineLeft()
		case 2:
			p.screen.ClearLineAll()
		}
	case 'A':
		n := 1
		if len(params) > 0 && params[0] > 0 {
			n = params[0]
		}
		p.screen.CursorUp(n)
	case 'B':
		n := 1
		if len(params) > 0 && params[0] > 0 {
			n = params[0]
		}
		p.screen.CursorDown(n)
	case 'C':
		n := 1
		if len(params) > 0 && params[0] > 0 {
			n = params[0]
		}
		p.screen.CursorForward(n)
	case 'D':
		n := 1
		if len(params) > 0 && params[0] > 0 {
			n = params[0]
		}
		p.screen.CursorBackward(n)
	case 'E':
		n := 1
		if len(params) > 0 && params[0] > 0 {
			n = params[0]
		}
		p.screen.CursorNextLine(n)
	case 'F':
		n := 1
		if len(params) > 0 && params[0] > 0 {
			n = params[0]
		}
		p.screen.CursorPrevLine(n)
	case 'G':
		col := 1
		if len(params) > 0 && params[0] > 0 {
			col = params[0]
		}
		p.screen.SetCursor(p.screen.Cursor.Row, col-1)
	case 'd':
		row := 1
		if len(params) > 0 && params[0] > 0 {
			row = params[0]
		}
		p.screen.SetCursorAddress(row-1, p.screen.Cursor.Col)
	case '@':
		p.screen.InsertChars(firstParam(params, 1))
	case 'P':
		p.screen.DeleteChars(firstParam(params, 1))
	case 'X':
		p.screen.EraseChars(firstParam(params, 1))
	case 'L':
		p.screen.InsertLines(firstParam(params, 1))
	case 'M':
		p.screen.DeleteLines(firstParam(params, 1))
	case 'S':
		p.screen.ScrollRegionUp(firstParam(params, 1))
	case 'T':
		p.screen.ScrollRegionDown(firstParam(params, 1))
	case 's':
		// Save cursor (DECSC) — only standard sequences.
		if !isPrivate {
			p.saveCursorState()
		}
	case 'u':
		// Restore cursor (DECRC) — only standard sequences.
		// Private forms like CSI ? u or CSI > 7 u are kitty keyboard protocol.
		if !isPrivate {
			p.restoreCursorState()
		}
	case 'b':
		if !isPrivate {
			p.screen.RepeatPrevious(firstParam(params, 1))
		}
	case 'n':
		if !isPrivate {
			n := firstParam(params, 0)
			switch n {
			case 5:
				p.respond([]byte("\x1b[0n"))
			case 6:
				row, col := p.screen.CursorPos()
				p.respond([]byte(fmt.Sprintf("\x1b[%d;%dR", row+1, col+1)))
			}
		}
	case 'c':
		if privateMarker == '>' {
			p.respond([]byte("\x1b[>0;1;0c"))
		} else if !isPrivate {
			p.respond([]byte("\x1b[?1;2c"))
		}
	case 'r':
		// Set scroll region (DECSTBM)
		top := 1
		bottom := p.screen.Rows
		if len(params) > 0 {
			top = params[0]
		}
		if len(params) > 1 {
			bottom = params[1]
		}
		p.screen.SetScrollRegion(top, bottom)
	case 'h':
		if isPrivate {
			for _, mode := range params {
				p.setPrivateMode(mode, true)
			}
		}
	case 'l':
		if isPrivate {
			for _, mode := range params {
				p.setPrivateMode(mode, false)
			}
		}
	case '~':
		// Bracketed-paste delimiters belong to terminal input. If an
		// application echoes them back, consume the control sequence without
		// changing output parsing state.
	}
}

func (p *Parser) handleEscapeIntermediate(intermediate, final byte) {
	lineDrawing := final == '0'
	if final != '0' && final != 'B' {
		return
	}
	switch intermediate {
	case '(':
		p.g0LineDrawing = lineDrawing
	case ')':
		p.g1LineDrawing = lineDrawing
	}
}

func (p *Parser) setPrivateMode(mode int, active bool) {
	switch mode {
	case 1:
		p.screen.applicationCursor = active
	case 6:
		p.screen.SetOriginMode(active)
	case 7:
		p.screen.SetAutoWrap(active)
	case 25:
		p.screen.CursorVisible = active
	case 1049:
		if active {
			p.screen.EnterAltScreen()
		} else {
			p.screen.ExitAltScreen()
		}
	case 1000:
		p.screen.mouseMode1000 = active
	case 1002:
		p.screen.mouseMode1002 = active
	case 1003:
		p.screen.mouseMode1003 = active
	case 1004:
		p.screen.focusReporting = active
	case 1006:
		p.screen.mouseSGR = active
	case 2004:
		p.screen.bracketedPaste = active
	case 2026:
		p.screen.SetSync(active)
	}
}

func firstParam(params []int, defaultValue int) int {
	if len(params) == 0 || params[0] <= 0 {
		return defaultValue
	}
	return params[0]
}

var decSpecialGraphics = map[byte]rune{
	'`': '◆', 'a': '▒', 'b': '␉', 'c': '␌', 'd': '␍', 'e': '␊',
	'f': '°', 'g': '±', 'h': '␤', 'i': '␋', 'j': '┘', 'k': '┐',
	'l': '┌', 'm': '└', 'n': '┼', 'o': '⎺', 'p': '⎻', 'q': '─',
	'r': '⎼', 's': '⎽', 't': '├', 'u': '┤', 'v': '┴', 'w': '┬',
	'x': '│', 'y': '≤', 'z': '≥', '{': 'π', '|': '≠', '}': '£', '~': '·',
}

func decSpecialGraphic(b byte) rune {
	if r, ok := decSpecialGraphics[b]; ok {
		return r
	}
	return rune(b)
}

func parseParams(s string) []int {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ";")
	var out []int
	for _, part := range parts {
		if part == "" {
			out = append(out, 0)
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			out = append(out, 0)
			continue
		}
		out = append(out, n)
	}
	return out
}

func (p *Parser) clearFromCursor() {
	row := p.screen.Cursor.Row
	col := p.screen.Cursor.Col
	p.screen.clearCellRange(row, col, p.screen.Cols)
	for r := row + 1; r < p.screen.Rows; r++ {
		clear(p.screen.Cells[r])
	}
}

func (p *Parser) handleSGR(params []int) {
	if len(params) == 0 {
		params = []int{0}
	}
	for i := 0; i < len(params); i++ {
		code := params[i]
		switch {
		case code == 0:
			p.screen.Cursor.FG = ""
			p.screen.Cursor.BG = ""
			p.screen.Cursor.Style = 0
		case code == 1:
			p.screen.Cursor.Style |= StyleBold
		case code == 2:
			p.screen.Cursor.Style |= StyleDim
		case code == 3:
			p.screen.Cursor.Style |= StyleItalic
		case code == 4:
			p.screen.Cursor.Style |= StyleUnderline
		case code == 5:
			p.screen.Cursor.Style |= StyleBlink
		case code == 7:
			p.screen.Cursor.Style |= StyleReverse
		case code == 8:
			p.screen.Cursor.Style |= StyleHidden
		case code == 9:
			p.screen.Cursor.Style |= StyleStrikethrough
		case code == 22:
			p.screen.Cursor.Style &^= StyleBold | StyleDim
		case code == 23:
			p.screen.Cursor.Style &^= StyleItalic
		case code == 24:
			p.screen.Cursor.Style &^= StyleUnderline
		case code == 25:
			p.screen.Cursor.Style &^= StyleBlink
		case code == 27:
			p.screen.Cursor.Style &^= StyleReverse
		case code == 28:
			p.screen.Cursor.Style &^= StyleHidden
		case code == 29:
			p.screen.Cursor.Style &^= StyleStrikethrough
		case code >= 30 && code <= 37:
			p.screen.Cursor.FG = ansi256Color(code - 30)
		case code == 38:
			if i+2 < len(params) && params[i+1] == 5 {
				p.screen.Cursor.FG = ansi256Color(params[i+2])
				i += 2
			} else if i+4 < len(params) && params[i+1] == 2 {
				p.screen.Cursor.FG = lipgloss.Color(rgb(params[i+2], params[i+3], params[i+4]))
				i += 4
			}
		case code == 39:
			p.screen.Cursor.FG = ""
		case code >= 40 && code <= 47:
			p.screen.Cursor.BG = ansi256Color(code - 40)
		case code == 48:
			if i+2 < len(params) && params[i+1] == 5 {
				p.screen.Cursor.BG = ansi256Color(params[i+2])
				i += 2
			} else if i+4 < len(params) && params[i+1] == 2 {
				p.screen.Cursor.BG = lipgloss.Color(rgb(params[i+2], params[i+3], params[i+4]))
				i += 4
			}
		case code == 49:
			p.screen.Cursor.BG = ""
		case code >= 90 && code <= 97:
			p.screen.Cursor.FG = ansi256Color(code - 90 + 8)
		case code >= 100 && code <= 107:
			p.screen.Cursor.BG = ansi256Color(code - 100 + 8)
		}
	}
}

func ansi256Color(n int) lipgloss.Color {
	if n < 0 || n > 255 {
		return ""
	}
	if n < 16 {
		// Standard colors
		colors := []string{
			"#000000", "#800000", "#008000", "#808000",
			"#000080", "#800080", "#008080", "#c0c0c0",
			"#808080", "#ff0000", "#00ff00", "#ffff00",
			"#0000ff", "#ff00ff", "#00ffff", "#ffffff",
		}
		return lipgloss.Color(colors[n])
	}
	if n < 232 {
		// xterm 6x6x6 color cube: 0, 95, 135, 175, 215, 255.
		levels := [...]int{0, 95, 135, 175, 215, 255}
		n -= 16
		r := n / 36
		g := (n / 6) % 6
		b := n % 6
		return lipgloss.Color(rgb(levels[r], levels[g], levels[b]))
	}
	// Grayscale
	v := (n-232)*10 + 8
	return lipgloss.Color(rgb(v, v, v))
}

func rgb(r, g, b int) string {
	return "#" + hexByte(r) + hexByte(g) + hexByte(b)
}

func hexByte(v int) string {
	if v < 0 {
		v = 0
	}
	if v > 255 {
		v = 255
	}
	h := strconv.FormatInt(int64(v), 16)
	if len(h) == 1 {
		return "0" + h
	}
	return h
}
