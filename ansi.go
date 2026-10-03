package portalis

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

// Parser parses ANSI escape sequences and updates a Screen. It is not safe for
// concurrent use; callbacks run synchronously during Feed and must not re-enter
// the parser.
type Parser struct {
	screen                *Screen
	state                 ansiState
	buf                   strings.Builder
	utf8Buf               []byte
	onCWD                 func(WorkingDirectory)
	onTitle               func(string)
	onResponse            func([]byte)
	lastCWD               WorkingDirectory
	lastTitle             string
	escapeIntermediate    byte
	g0LineDrawing         bool
	g1LineDrawing         bool
	useG1                 bool
	savedG0LineDrawing    bool
	savedG1LineDrawing    bool
	savedUseG1            bool
	altSavedG0LineDrawing bool
	altSavedG1LineDrawing bool
	altSavedUseG1         bool
	altCharsetSaved       bool
	dec1049SavedCursor    screenCursorState
	dec1049SavedG0        bool
	dec1049SavedG1        bool
	dec1049SavedUseG1     bool
	dec1049CursorSaved    bool
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

// WorkingDirectory is a validated OSC 7 location. Host is preserved for remote
// file URLs; Local reports whether Path belongs to this machine.
type WorkingDirectory struct {
	Host  string
	Path  string
	Local bool
}

// SetCWDCallback sets a callback invoked synchronously by Feed for OSC 7 changes.
// The callback must return promptly and must not re-enter the parser.
func (p *Parser) SetCWDCallback(fn func(WorkingDirectory)) {
	p.onCWD = fn
}

// SetTitleCallback receives OSC 0/2 titles synchronously during Feed.
func (p *Parser) SetTitleCallback(fn func(string)) {
	p.onTitle = fn
}

// SetResponseCallback receives terminal-generated replies synchronously during
// Feed, such as DSR/DA. The callback must not block or re-enter the parser;
// Emulator queues replies and writes them after releasing its state mutex.
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

func (p *Parser) saveDEC1049CursorState() {
	p.dec1049SavedCursor = p.screen.cursorState()
	p.dec1049SavedG0 = p.g0LineDrawing
	p.dec1049SavedG1 = p.g1LineDrawing
	p.dec1049SavedUseG1 = p.useG1
	p.dec1049CursorSaved = true
}

func (p *Parser) restoreDEC1049CursorState() {
	if !p.dec1049CursorSaved {
		return
	}
	p.screen.restoreCursorState(p.dec1049SavedCursor)
	p.g0LineDrawing = p.dec1049SavedG0
	p.g1LineDrawing = p.dec1049SavedG1
	p.useG1 = p.dec1049SavedUseG1
	p.dec1049SavedCursor = screenCursorState{}
	p.dec1049SavedG0 = false
	p.dec1049SavedG1 = false
	p.dec1049SavedUseG1 = false
	p.dec1049CursorSaved = false
}

// Reset restores power-on screen and parser state at the current dimensions.
func (p *Parser) Reset() {
	p.screen.Reset()
	p.resetCharsets()
	p.state = stateNormal
	p.buf.Reset()
	p.utf8Buf = nil
	p.escapeIntermediate = 0
	p.lastCWD = WorkingDirectory{}
	p.lastTitle = ""
}

func (p *Parser) resetCharsets() {
	p.g0LineDrawing = false
	p.g1LineDrawing = false
	p.useG1 = false
	p.savedG0LineDrawing = false
	p.savedG1LineDrawing = false
	p.savedUseG1 = false
	p.altSavedG0LineDrawing = false
	p.altSavedG1LineDrawing = false
	p.altSavedUseG1 = false
	p.altCharsetSaved = false
	p.dec1049SavedCursor = screenCursorState{}
	p.dec1049SavedG0 = false
	p.dec1049SavedG1 = false
	p.dec1049SavedUseG1 = false
	p.dec1049CursorSaved = false
}

func (p *Parser) softReset() {
	p.screen.SoftReset()
	p.resetCharsets()
	p.state = stateNormal
	p.buf.Reset()
	p.utf8Buf = nil
	p.escapeIntermediate = 0
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
			p.screen.markDirty()
			p.screen.cursor.Col = 0
			p.screen.wrapPending = false
			return
		}
		if b == '\n' {
			p.flushUtf8()
			p.screen.markDirty()
			// If a wrap is pending, LF behaves like CR+LF.
			if p.screen.wrapPending {
				p.screen.wrapPending = false
				p.screen.cursor.Col = 0
			}
			p.screen.Index()
			return
		}
		if b == '\t' {
			p.flushUtf8()
			p.screen.ClearWrapPending()
			p.screen.TabForward(1)
			return
		}
		if b == '\b' {
			p.flushUtf8()
			p.screen.markDirty()
			p.screen.wrapPending = false
			if p.screen.cursor.Col > 0 {
				p.screen.cursor.Col--
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
		case 'H':
			p.screen.SetTabStop()
		case 'c':
			p.Reset()
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
	if len(parts) != 2 {
		return
	}
	switch parts[0] {
	case "0", "2":
		title, ok := sanitizeTerminalTitle(parts[1])
		if ok && title != p.lastTitle {
			p.lastTitle = title
			if p.onTitle != nil {
				p.onTitle(title)
			}
		}
	case "7":
		cwd, ok := parseOSC7WorkingDirectory(parts[1])
		if ok && cwd != p.lastCWD {
			p.lastCWD = cwd
			if p.onCWD != nil {
				p.onCWD(cwd)
			}
		}
	}
}

const maxTerminalTitleBytes = 4096

func sanitizeTerminalTitle(title string) (string, bool) {
	if len(title) > maxTerminalTitleBytes || !utf8.ValidString(title) {
		return "", false
	}
	for _, r := range title {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return "", false
		}
	}
	return title, true
}

func parseOSC7WorkingDirectory(raw string) (WorkingDirectory, bool) {
	var host, path string
	local := true
	if strings.HasPrefix(strings.ToLower(raw), "file:") {
		u, err := url.Parse(raw)
		if err != nil || !strings.EqualFold(u.Scheme, "file") || u.Opaque != "" || u.User != nil || u.Port() != "" ||
			u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return WorkingDirectory{}, false
		}
		host = strings.ToLower(u.Hostname())
		local = isLocalOSC7Host(host)
		path = u.Path
	} else {
		path = raw
	}
	if !filepath.IsAbs(path) {
		return WorkingDirectory{}, false
	}
	for _, r := range path {
		if r == 0 || r == '\x1b' || r == '\x07' || r == '\r' || r == '\n' {
			return WorkingDirectory{}, false
		}
	}
	return WorkingDirectory{Host: host, Path: path, Local: local}, true
}

// extractOSC7Path returns only local paths for legacy package-internal callers.
func extractOSC7Path(raw string) string {
	cwd, ok := parseOSC7WorkingDirectory(raw)
	if !ok || !cwd.Local {
		return ""
	}
	return cwd.Path
}

func isLocalOSC7Host(host string) bool {
	if host == "" || strings.EqualFold(host, "localhost") || strings.EqualFold(host, "localhost.localdomain") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	localHost, err := os.Hostname()
	return err == nil && strings.EqualFold(host, localHost)
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
		p.handleSGRSequence(rawParams)
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
		p.screen.SetCursor(p.screen.cursor.Row, col-1)
	case 'd':
		row := 1
		if len(params) > 0 && params[0] > 0 {
			row = params[0]
		}
		p.screen.SetCursorAddress(row-1, p.screen.cursor.Col)
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
	case 'I':
		if !isPrivate {
			p.screen.ClearWrapPending()
			p.screen.TabForward(firstParam(params, 1))
		}
	case 'Z':
		if !isPrivate {
			p.screen.TabBackward(firstParam(params, 1))
		}
	case 'g':
		if !isPrivate {
			p.screen.ClearTabStop(firstParam(params, 0))
		}
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
			p.respond([]byte("\x1b[>0;0;0c"))
		} else if !isPrivate {
			p.respond([]byte("\x1b[?1;2c"))
		}
	case 'r':
		// Set scroll region (DECSTBM)
		top := 1
		bottom := p.screen.rows
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
		} else {
			for _, mode := range params {
				if mode == 4 {
					p.screen.SetInsertMode(true)
				}
			}
		}
	case 'l':
		if isPrivate {
			for _, mode := range params {
				p.setPrivateMode(mode, false)
			}
		} else {
			for _, mode := range params {
				if mode == 4 {
					p.screen.SetInsertMode(false)
				}
			}
		}
	case 'p':
		if privateMarker == '!' {
			p.softReset()
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
		if p.screen.cursorVisible != active {
			p.screen.markDirty()
			p.screen.cursorVisible = active
		}
	case 1047:
		if active {
			p.enterAltScreen()
		} else {
			p.exitAltScreen()
		}
	case 1049:
		if active {
			if !p.screen.altScreen {
				p.saveDEC1049CursorState()
				p.enterAltScreen()
			}
		} else {
			p.exitAltScreen()
			p.restoreDEC1049CursorState()
		}
	case 1048:
		if active {
			p.saveCursorState()
		} else {
			p.restoreCursorState()
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

func (p *Parser) enterAltScreen() {
	if p.screen.altScreen {
		return
	}
	p.altSavedG0LineDrawing = p.g0LineDrawing
	p.altSavedG1LineDrawing = p.g1LineDrawing
	p.altSavedUseG1 = p.useG1
	p.altCharsetSaved = true
	p.screen.EnterAltScreen()
}

func (p *Parser) exitAltScreen() {
	if !p.screen.altScreen {
		return
	}
	p.screen.ExitAltScreen()
	if p.altCharsetSaved {
		p.g0LineDrawing = p.altSavedG0LineDrawing
		p.g1LineDrawing = p.altSavedG1LineDrawing
		p.useG1 = p.altSavedUseG1
	}
	p.altCharsetSaved = false
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
	row := p.screen.cursor.Row
	col := p.screen.cursor.Col
	p.screen.clearCellRange(row, col, p.screen.cols)
	for r := row + 1; r < p.screen.rows; r++ {
		p.screen.fillBlank(p.screen.cells[r], 0, p.screen.cols)
	}
}

func (p *Parser) handleSGRSequence(rawParams string) {
	if !strings.Contains(rawParams, ":") {
		p.handleSGR(parseParams(rawParams))
		return
	}
	groups := strings.Split(rawParams, ";")
	for i := 0; i < len(groups); {
		rawGroup := groups[i]
		if !strings.Contains(rawGroup, ":") {
			params := parseParams(rawGroup)
			if len(params) == 0 {
				p.handleSGR(nil)
				i++
				continue
			}
			code := params[0]
			if code == 38 || code == 48 {
				if colorParams, consumed, ok := parseSemicolonSGRColor(groups, i, code); ok {
					p.handleSGR(colorParams)
					i += consumed + 1
					continue
				}
			}
			p.handleSGR(params)
			i++
			continue
		}
		fields := strings.Split(rawGroup, ":")
		code := 0
		if fields[0] != "" {
			parsed, err := strconv.Atoi(fields[0])
			if err != nil {
				i++
				continue
			}
			code = parsed
		}
		if code == 38 || code == 48 {
			color, ok := parseColonSGRColor(fields)
			if ok {
				if code == 38 {
					p.screen.cursor.FG = color
				} else {
					p.screen.cursor.BG = color
				}
			}
			i++
			continue
		}
		p.handleSGR([]int{code})
		i++
	}
}

func parseSemicolonSGRColor(groups []string, index, code int) ([]int, int, bool) {
	if index+2 >= len(groups) {
		return nil, 0, false
	}
	mode, err := strconv.Atoi(groups[index+1])
	if err != nil {
		return nil, 0, false
	}
	switch mode {
	case 5:
		value, err := strconv.Atoi(groups[index+2])
		if err != nil || value < 0 || value > 255 {
			return nil, 0, false
		}
		return []int{code, mode, value}, 2, true
	case 2:
		if index+4 >= len(groups) {
			return nil, 0, false
		}
		params := []int{code, mode}
		for _, raw := range groups[index+2 : index+5] {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 0 || value > 255 {
				return nil, 0, false
			}
			params = append(params, value)
		}
		return params, 4, true
	default:
		return nil, 0, false
	}
}

func parseColonSGRColor(fields []string) (lipgloss.Color, bool) {
	if len(fields) < 3 {
		return "", false
	}
	mode, err := strconv.Atoi(fields[1])
	if err != nil {
		return "", false
	}
	switch mode {
	case 5:
		if len(fields) != 3 {
			return "", false
		}
		value, err := strconv.Atoi(fields[2])
		if err != nil || value < 0 || value > 255 {
			return "", false
		}
		return ansi256Color(value), true
	case 2:
		start := 2
		if len(fields) == 6 {
			if fields[2] != "" && fields[2] != "0" {
				return "", false
			}
			start = 3
		}
		if len(fields) != start+3 {
			return "", false
		}
		channels := [3]int{}
		for i := range channels {
			value, err := strconv.Atoi(fields[start+i])
			if err != nil || value < 0 || value > 255 {
				return "", false
			}
			channels[i] = value
		}
		return lipgloss.Color(rgb(channels[0], channels[1], channels[2])), true
	default:
		return "", false
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
			p.screen.cursor.FG = ""
			p.screen.cursor.BG = ""
			p.screen.cursor.Style = 0
		case code == 1:
			p.screen.cursor.Style |= StyleBold
		case code == 2:
			p.screen.cursor.Style |= StyleDim
		case code == 3:
			p.screen.cursor.Style |= StyleItalic
		case code == 4:
			p.screen.cursor.Style |= StyleUnderline
		case code == 5:
			p.screen.cursor.Style |= StyleBlink
		case code == 7:
			p.screen.cursor.Style |= StyleReverse
		case code == 8:
			p.screen.cursor.Style |= StyleHidden
		case code == 9:
			p.screen.cursor.Style |= StyleStrikethrough
		case code == 10:
			p.useG1 = false
		case code == 11:
			p.useG1 = true
		case code == 22:
			p.screen.cursor.Style &^= StyleBold | StyleDim
		case code == 23:
			p.screen.cursor.Style &^= StyleItalic
		case code == 24:
			p.screen.cursor.Style &^= StyleUnderline
		case code == 25:
			p.screen.cursor.Style &^= StyleBlink
		case code == 27:
			p.screen.cursor.Style &^= StyleReverse
		case code == 28:
			p.screen.cursor.Style &^= StyleHidden
		case code == 29:
			p.screen.cursor.Style &^= StyleStrikethrough
		case code >= 30 && code <= 37:
			p.screen.cursor.FG = ansi256Color(code - 30)
		case code == 38:
			if i+2 < len(params) && params[i+1] == 5 {
				p.screen.cursor.FG = ansi256Color(params[i+2])
				i += 2
			} else if i+4 < len(params) && params[i+1] == 2 {
				p.screen.cursor.FG = lipgloss.Color(rgb(params[i+2], params[i+3], params[i+4]))
				i += 4
			}
		case code == 39:
			p.screen.cursor.FG = ""
		case code >= 40 && code <= 47:
			p.screen.cursor.BG = ansi256Color(code - 40)
		case code == 48:
			if i+2 < len(params) && params[i+1] == 5 {
				p.screen.cursor.BG = ansi256Color(params[i+2])
				i += 2
			} else if i+4 < len(params) && params[i+1] == 2 {
				p.screen.cursor.BG = lipgloss.Color(rgb(params[i+2], params[i+3], params[i+4]))
				i += 4
			}
		case code == 49:
			p.screen.cursor.BG = ""
		case code >= 90 && code <= 97:
			p.screen.cursor.FG = ansi256Color(code - 90 + 8)
		case code >= 100 && code <= 107:
			p.screen.cursor.BG = ansi256Color(code - 100 + 8)
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
