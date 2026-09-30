package portalis

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestParserPut(t *testing.T) {
	s := NewScreen(5, 10)
	p := NewParser(s)
	p.Feed([]byte("hello"))

	if s.cells[0][0].Rune != 'h' {
		t.Errorf("expected 'h', got %c", s.cells[0][0].Rune)
	}
	if s.cells[0][4].Rune != 'o' {
		t.Errorf("expected 'o', got %c", s.cells[0][4].Rune)
	}
}

func TestParserCursor(t *testing.T) {
	s := NewScreen(5, 10)
	p := NewParser(s)
	p.Feed([]byte("\x1b[3;5Hab"))

	if s.cells[2][4].Rune != 'a' {
		t.Errorf("expected 'a' at row 3 col 5, got %c", s.cells[2][4].Rune)
	}
}

func TestParserColor(t *testing.T) {
	s := NewScreen(2, 10)
	p := NewParser(s)
	p.Feed([]byte("\x1b[31mred\x1b[0m"))

	if s.cells[0][0].FG != lipgloss.Color("#800000") {
		t.Errorf("expected dark red fg, got %v", s.cells[0][0].FG)
	}
}

func TestParserClear(t *testing.T) {
	s := NewScreen(3, 10)
	p := NewParser(s)
	p.Feed([]byte("hello"))
	p.Feed([]byte("\x1b[2J"))

	if s.cells[0][0].Rune != 0 {
		t.Error("expected screen cleared")
	}
}

func TestParserNewline(t *testing.T) {
	s := NewScreen(3, 10)
	p := NewParser(s)
	p.Feed([]byte("hello\r\nworld"))

	if s.cells[1][0].Rune != 'w' {
		t.Errorf("expected 'w' on second line, got %c", s.cells[1][0].Rune)
	}
}

func TestColonSeparatedSGRColors(t *testing.T) {
	s := NewScreen(1, 8)
	p := NewParser(s)
	p.Feed([]byte("\x1b[38:2::12:34:56;48:5:196mA"))
	p.Feed([]byte("\x1b[38:2:1:2:3;48:2:0:4:5:6mB"))

	if got := s.cells[0][0].FG; got != lipgloss.Color("#0c2238") {
		t.Fatalf("colon RGB foreground = %q, want #0c2238", got)
	}
	if got := s.cells[0][0].BG; got != lipgloss.Color("#ff0000") {
		t.Fatalf("colon 256-color background = %q, want #ff0000", got)
	}
	if got := s.cells[0][1].FG; got != lipgloss.Color("#010203") {
		t.Fatalf("colon RGB without colorspace foreground = %q, want #010203", got)
	}
	if got := s.cells[0][1].BG; got != lipgloss.Color("#040506") {
		t.Fatalf("colon RGB colorspace background = %q, want #040506", got)
	}
}

func TestAnsi256Color(t *testing.T) {
	c := ansi256Color(9)
	if c != lipgloss.Color("#ff0000") {
		t.Errorf("expected bright red, got %v", c)
	}
}

func TestRGB(t *testing.T) {
	if rgb(255, 0, 128) != "#ff0080" {
		t.Errorf("expected #ff0080, got %s", rgb(255, 0, 128))
	}
}

func TestOSC7(t *testing.T) {
	s := NewScreen(10, 40)
	p := NewParser(s)

	var cwd WorkingDirectory
	p.SetCWDCallback(func(location WorkingDirectory) {
		cwd = location
	})

	p.Feed([]byte("before\x1b]7;/Users/a/foo\x1b\\after"))

	if cwd.Path != "/Users/a/foo" || !cwd.Local || cwd.Host != "" {
		t.Fatalf("cwd = %+v, want local /Users/a/foo", cwd)
	}

	text := s.Render()
	if !strings.Contains(text, "before") {
		t.Fatalf("screen missing 'before': %q", text)
	}
	if !strings.Contains(text, "after") {
		t.Fatalf("screen missing 'after': %q", text)
	}
}

func TestParserTmuxCharsetSelection(t *testing.T) {
	s := NewScreen(2, 12)
	p := NewParser(s)

	p.Feed([]byte("\x1b(Bhello"))
	p.Feed([]byte("\r\n\x1b(0lqk\x1b(B"))

	if got := s.RenderLine(0); got != "hello       " {
		t.Fatalf("ASCII charset rendered %q, want %q", got, "hello       ")
	}
	if got := s.RenderLine(1); got != "┌─┐         " {
		t.Fatalf("DEC graphics rendered %q, want %q", got, "┌─┐         ")
	}
}

func TestParserTmuxDeleteLineAndReverseIndex(t *testing.T) {
	t.Run("CSI M deletes a line", func(t *testing.T) {
		s := NewScreen(4, 4)
		p := NewParser(s)
		p.Feed([]byte("1111\r\n2222\r\n3333\r\n4444"))
		p.Feed([]byte("\x1b[2;1H\x1b[M"))

		want := []string{"1111", "3333", "4444", "    "}
		for row, expected := range want {
			if got := s.RenderLine(row); got != expected {
				t.Fatalf("row %d = %q, want %q", row, got, expected)
			}
		}
	})

	t.Run("ESC M performs reverse index", func(t *testing.T) {
		s := NewScreen(3, 4)
		p := NewParser(s)
		p.Feed([]byte("AAAA\r\nBBBB\r\nCCCC"))
		p.Feed([]byte("\x1b[1;1H\x1bM"))

		want := []string{"    ", "AAAA", "BBBB"}
		for row, expected := range want {
			if got := s.RenderLine(row); got != expected {
				t.Fatalf("row %d = %q, want %q", row, got, expected)
			}
		}
	})
}

func TestParserTmuxCharacterEditing(t *testing.T) {
	tests := []struct {
		name string
		seq  string
		want string
	}{
		{name: "insert characters", seq: "\x1b[1;3H\x1b[2@", want: "AB  C"},
		{name: "delete characters", seq: "\x1b[1;3H\x1b[2P", want: "ABE  "},
		{name: "erase characters", seq: "\x1b[1;3H\x1b[2X", want: "AB  E"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewScreen(1, 5)
			p := NewParser(s)
			p.Feed([]byte("ABCDE" + tt.seq))
			if got := s.RenderLine(0); got != tt.want {
				t.Fatalf("line = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParserTmuxLineEditingWithinScrollRegion(t *testing.T) {
	newFixture := func() (*Screen, *Parser) {
		s := NewScreen(5, 3)
		p := NewParser(s)
		p.Feed([]byte("111\r\n222\r\n333\r\n444\r\n555"))
		p.Feed([]byte("\x1b[2;4r"))
		return s, p
	}

	t.Run("insert line", func(t *testing.T) {
		s, p := newFixture()
		p.Feed([]byte("\x1b[3;1H\x1b[L"))
		want := []string{"111", "222", "   ", "333", "555"}
		for row, expected := range want {
			if got := s.RenderLine(row); got != expected {
				t.Fatalf("row %d = %q, want %q", row, got, expected)
			}
		}
	})

	t.Run("scroll up then down", func(t *testing.T) {
		s, p := newFixture()
		p.Feed([]byte("\x1b[S"))
		wantUp := []string{"111", "333", "444", "   ", "555"}
		for row, expected := range wantUp {
			if got := s.RenderLine(row); got != expected {
				t.Fatalf("after SU row %d = %q, want %q", row, got, expected)
			}
		}

		p.Feed([]byte("\x1b[T"))
		wantDown := []string{"111", "   ", "333", "444", "555"}
		for row, expected := range wantDown {
			if got := s.RenderLine(row); got != expected {
				t.Fatalf("after SD row %d = %q, want %q", row, got, expected)
			}
		}
	})
}

func TestParserTmuxModesAndFrame(t *testing.T) {
	s := NewScreen(3, 16)
	p := NewParser(s)
	frame := "\x1b[?1;25;2004h\x1b[?25l\x1b(B\x1b[2J\x1b[Htmux\x1b[2;1Hstatus\x1b[?25h"
	p.Feed([]byte(frame))

	if !s.applicationCursor {
		t.Fatal("application cursor mode was not enabled")
	}
	if !s.bracketedPaste {
		t.Fatal("bracketed paste mode was not enabled")
	}
	if !s.cursorVisible {
		t.Fatal("cursor should be visible after DECSET ?25")
	}
	if got := s.RenderLine(0); got != "tmux            " {
		t.Fatalf("frame row 0 = %q", got)
	}
	if got := s.RenderLine(1); got != "status          " {
		t.Fatalf("frame row 1 = %q", got)
	}
}

func TestParserInvalidUTF8DoesNotAccumulate(t *testing.T) {
	s := NewScreen(2, 20)
	p := NewParser(s)
	p.Feed(bytes.Repeat([]byte{0xff}, 1024))
	if len(p.utf8Buf) != 0 {
		t.Fatalf("invalid UTF-8 buffer retained %d bytes", len(p.utf8Buf))
	}
}

func TestParserFlushesIncompleteUTF8BeforeASCII(t *testing.T) {
	s := NewScreen(1, 10)
	p := NewParser(s)
	p.Feed([]byte{0xc2})
	p.Feed([]byte("A"))
	if len(p.utf8Buf) != 0 {
		t.Fatalf("incomplete UTF-8 buffer retained %d bytes", len(p.utf8Buf))
	}
	if got := s.cells[0][1].Rune; got != 'A' {
		t.Fatalf("ASCII after incomplete UTF-8 = %q, want A", got)
	}
}

func TestParserBoundsUnterminatedOSC(t *testing.T) {
	s := NewScreen(1, 10)
	p := NewParser(s)
	payload := append([]byte("\x1b]7;"), bytes.Repeat([]byte{'x'}, maxOSCSequenceBytes+1024)...)
	p.Feed(payload)
	if p.buf.Len() > maxOSCSequenceBytes {
		t.Fatalf("OSC buffer grew to %d bytes", p.buf.Len())
	}
}

func TestParserBoundsUnterminatedCSI(t *testing.T) {
	s := NewScreen(1, 10)
	p := NewParser(s)
	payload := append([]byte("\x1b["), bytes.Repeat([]byte{'1'}, maxCSISequenceBytes+1024)...)
	p.Feed(payload)
	if p.buf.Len() > maxCSISequenceBytes {
		t.Fatalf("CSI buffer grew to %d bytes", p.buf.Len())
	}
}

func TestANSI256ColorXtermCube(t *testing.T) {
	tests := map[int]string{
		16:  "#000000",
		17:  "#00005f",
		21:  "#0000ff",
		196: "#ff0000",
		231: "#ffffff",
	}
	for index, want := range tests {
		if got := string(ansi256Color(index)); got != want {
			t.Errorf("ansi256Color(%d) = %q, want %q", index, got, want)
		}
	}
}

func TestParserMousePrivateModes(t *testing.T) {
	s := NewScreen(2, 10)
	p := NewParser(s)
	p.Feed([]byte("\x1b[?1002;1006h"))
	if s.mouseTrackingMode() != 1002 || !s.mouseSGR {
		t.Fatalf("mouse modes not enabled: tracking=%d sgr=%v", s.mouseTrackingMode(), s.mouseSGR)
	}
	p.Feed([]byte("\x1b[?1002;1006l"))
	if s.mouseTrackingMode() != 0 || s.mouseSGR {
		t.Fatalf("mouse modes not disabled: tracking=%d sgr=%v", s.mouseTrackingMode(), s.mouseSGR)
	}
}

func TestAltScreenRestoresCharset(t *testing.T) {
	s := NewScreen(2, 8)
	p := NewParser(s)
	p.Feed([]byte("\x1b(B\x1b[?1049h\x1b(0\x1b[?1049lq"))
	if got := s.LineText(0); got != "q" {
		t.Fatalf("charset after leaving alt screen rendered %q, want ASCII q", got)
	}
}

func TestOSC7PercentDecodedAndControlsRejected(t *testing.T) {
	if got := extractOSC7Path("file://localhost/tmp/a%20b"); got != "/tmp/a b" {
		t.Fatalf("decoded OSC 7 path = %q, want /tmp/a b", got)
	}
	if got := extractOSC7Path("file:///tmp/a%20b"); got != "/tmp/a b" {
		t.Fatalf("decoded hostless OSC 7 path = %q, want /tmp/a b", got)
	}
	if got := extractOSC7Path("file://127.0.0.1/tmp/local"); got != "/tmp/local" {
		t.Fatalf("loopback OSC 7 path = %q, want /tmp/local", got)
	}
	for _, uri := range []string{
		"file://portalis-remote.invalid/tmp/remote",
		"file://user@localhost/tmp/userinfo",
		"file://localhost:22/tmp/port",
		"file://localhost/tmp/path?query",
		"file://localhost/tmp/path#fragment",
	} {
		if got := extractOSC7Path(uri); got != "" {
			t.Errorf("non-local or non-path OSC 7 URI %q accepted as %q", uri, got)
		}
	}
	if got := extractOSC7Path("file://localhost/tmp/bad%1Bname"); got != "" {
		t.Fatalf("OSC 7 control path accepted: %q", got)
	}
	if got := extractOSC7Path("/tmp/bad\nname"); got != "" {
		t.Fatalf("raw OSC 7 newline path accepted: %q", got)
	}
}

func TestOSC7PreservesRemoteAuthority(t *testing.T) {
	cwd, ok := parseOSC7WorkingDirectory("file://build-host.example/work/project%20one")
	if !ok || cwd.Local || cwd.Host != "build-host.example" || cwd.Path != "/work/project one" {
		t.Fatalf("remote OSC 7 location = %+v, %v", cwd, ok)
	}
	local, ok := parseOSC7WorkingDirectory("/tmp/trailing ")
	if !ok || local.Path != "/tmp/trailing " {
		t.Fatalf("OSC 7 local path lost trailing space: %+v, %v", local, ok)
	}
	if got := extractOSC7Path("file://build-host.example/work/project"); got != "" {
		t.Fatalf("legacy local-only extractor accepted remote path %q", got)
	}
}

func TestOSC0And2TitleCallbacks(t *testing.T) {
	parser := NewParser(NewScreen(1, 10))
	var titles []string
	parser.SetTitleCallback(func(title string) { titles = append(titles, title) })
	parser.Feed([]byte("\x1b]0;Build 🛠\x07\x1b]2;Build 🛠\x1b\\"))
	if len(titles) != 1 || titles[0] != "Build 🛠" {
		t.Fatalf("title callbacks = %#v, want one validated title", titles)
	}
	parser.Feed([]byte("\x1b]2;bad\x1btitle\x07\x1b]2;\nInjected\x07"))
	if len(titles) != 1 {
		t.Fatalf("control-containing title was accepted: %#v", titles)
	}
}

func TestDSRRepliesUseActualCursorAndSurviveSplitCSI(t *testing.T) {
	s := NewScreen(6, 10)
	p := NewParser(s)
	s.SetCursor(2, 4)

	var replies []string
	p.SetResponseCallback(func(data []byte) {
		replies = append(replies, string(data))
	})

	p.Feed([]byte("\x1b["))
	p.Feed([]byte("6n"))
	p.Feed([]byte("\x1b[5n"))

	want := []string{"\x1b[3;5R", "\x1b[0n"}
	if len(replies) != len(want) {
		t.Fatalf("DSR replies = %#v, want %#v", replies, want)
	}
	for i := range want {
		if replies[i] != want[i] {
			t.Fatalf("reply %d = %q, want %q", i, replies[i], want[i])
		}
	}
}

func TestDeviceAttributesReplies(t *testing.T) {
	s := NewScreen(2, 10)
	p := NewParser(s)
	var replies []string
	p.SetResponseCallback(func(data []byte) { replies = append(replies, string(data)) })

	p.Feed([]byte("\x1b[c\x1b[>c"))
	want := []string{"\x1b[?1;2c", "\x1b[>0;0;0c"}
	if len(replies) != len(want) || replies[0] != want[0] || replies[1] != want[1] {
		t.Fatalf("DA replies = %#v, want %#v", replies, want)
	}
}

func TestParserTabStopsAndInsertMode(t *testing.T) {
	screen := NewScreen(1, 16)
	parser := NewParser(screen)
	parser.Feed([]byte("\x1b[9G\x1b[0g\x1b[4G\x1bH\x1b[1G\t"))
	if _, col := screen.CursorPos(); col != 3 {
		t.Fatalf("custom tab stop moved cursor to %d, want 3", col)
	}
	parser.Feed([]byte("\x1b[3g\x1b[1G\t"))
	if _, col := screen.CursorPos(); col != 15 {
		t.Fatalf("tab with all stops cleared moved cursor to %d, want last column 15", col)
	}
	parser.Feed([]byte("\x1b[1G\x1b[I"))
	if _, col := screen.CursorPos(); col != 15 {
		t.Fatalf("CSI I with all stops cleared moved cursor to %d, want last column 15", col)
	}

	screen = NewScreen(1, 6)
	parser = NewParser(screen)
	parser.Feed([]byte("abcd\r\x1b[4hXY"))
	if got := screen.LineText(0); got != "XYabcd" {
		t.Fatalf("insert mode line = %q, want XYabcd", got)
	}
	parser.Feed([]byte("\x1b[4l\rZ"))
	if got := screen.LineText(0); got != "ZYabcd" {
		t.Fatalf("replace mode line = %q, want ZYabcd", got)
	}
}

func TestDEC1047AlternateBufferAndDEC1048CursorSave(t *testing.T) {
	screen := NewScreen(2, 8)
	parser := NewParser(screen)
	parser.Feed([]byte("main\x1b[?1047halt\x1b[?1047l"))
	if screen.altScreen || screen.LineText(0) != "main" {
		t.Fatalf("1047 did not restore main buffer: alt=%v line=%q", screen.altScreen, screen.LineText(0))
	}

	screen.SetCursor(0, 3)
	parser.Feed([]byte("\x1b[?1048h\x1b[2;6H\x1b[?1048l"))
	if row, col := screen.CursorPos(); row != 0 || col != 3 {
		t.Fatalf("1048 restored cursor to (%d,%d), want (0,3)", row, col)
	}
}

func TestANSITerminfoAlternateCharsetSGR(t *testing.T) {
	screen := NewScreen(1, 4)
	parser := NewParser(screen)
	parser.Feed([]byte("\x1b)0\x1b[11mq\x1b[10mq"))
	if got := screen.LineText(0); got != "─q" {
		t.Fatalf("SGR alternate charset output = %q, want ─q", got)
	}
}

func TestParserSoftResetAndRIS(t *testing.T) {
	screen := NewScreen(2, 8)
	parser := NewParser(screen)
	parser.Feed([]byte("keep\x1b[?7l\x1b[?2004h\x1b[?25l\x1b[4h\x1b[!p"))
	if got := screen.LineText(0); got != "keep" {
		t.Fatalf("DECSTR cleared text: %q", got)
	}
	if !screen.autoWrap || screen.bracketedPaste || !screen.cursorVisible || screen.insertMode {
		t.Fatalf("DECSTR left terminal modes active: wrap=%v paste=%v cursor=%v insert=%v", screen.autoWrap, screen.bracketedPaste, screen.cursorVisible, screen.insertMode)
	}

	parser.Feed([]byte("\x1b[?2004h\x1b[?1049halt"))
	parser.Feed([]byte("\x1bc"))
	if screen.LineText(0) != "" || screen.altScreen || screen.bracketedPaste || !screen.cursorVisible {
		t.Fatalf("RIS did not restore power-on state: line=%q alt=%v paste=%v cursor=%v", screen.LineText(0), screen.altScreen, screen.bracketedPaste, screen.cursorVisible)
	}
	if rows, cols := screen.Rows(), screen.Cols(); rows != 2 || cols != 8 {
		t.Fatalf("RIS changed dimensions to %dx%d", rows, cols)
	}
}

func TestBracketedPasteMarkersInOutputDoNotEnterPasteState(t *testing.T) {
	s := NewScreen(2, 20)
	p := NewParser(s)
	p.Feed([]byte("\x1b[200~é\x1b[201~X"))
	if got := s.LineText(0); got != "éX" {
		t.Fatalf("output around bracketed-paste markers = %q, want éX", got)
	}
	if p.state != stateNormal {
		t.Fatalf("parser state = %v, want normal", p.state)
	}
}

func TestStringControlsAreIgnoredUntilST(t *testing.T) {
	tests := []string{"P", "X", "^", "_"}
	for _, introducer := range tests {
		t.Run(introducer, func(t *testing.T) {
			s := NewScreen(1, 30)
			p := NewParser(s)
			payload := append([]byte("before\x1b"+introducer+"hidden payload\x1b"), '\\')
			payload = append(payload, []byte("after")...)
			p.Feed(payload)
			if got := s.LineText(0); got != "beforeafter" {
				t.Fatalf("string control leaked payload: %q", got)
			}
		})
	}
}

func TestED2ClearsWithoutHomingCursor(t *testing.T) {
	s := NewScreen(4, 10)
	p := NewParser(s)
	p.Feed([]byte("\x1b[3;5Hhello\x1b[2J"))
	row, col := s.CursorPos()
	if row != 2 || col != 9 {
		t.Fatalf("cursor after ED2 = %d,%d, want 2,9", row, col)
	}
	if got := strings.TrimSpace(s.RenderLine(2)); got != "" {
		t.Fatalf("screen not cleared by ED2: %q", got)
	}
}

func TestOriginModeMakesCUPRelativeToScrollRegion(t *testing.T) {
	s := NewScreen(6, 10)
	p := NewParser(s)
	p.Feed([]byte("\x1b[2;5r\x1b[?6h\x1b[1;1H"))
	if row, col := s.CursorPos(); row != 1 || col != 0 {
		t.Fatalf("origin-mode CUP = %d,%d, want 1,0", row, col)
	}
	p.Feed([]byte("\x1b[?6l"))
	if row, col := s.CursorPos(); row != 0 || col != 0 {
		t.Fatalf("DECOM reset cursor = %d,%d, want 0,0", row, col)
	}
}

func TestAutoWrapModeCanBeDisabled(t *testing.T) {
	s := NewScreen(1, 3)
	p := NewParser(s)
	p.Feed([]byte("\x1b[?7lABCD"))
	if got := s.RenderLine(0); got != "ABD" {
		t.Fatalf("DECAWM-off line = %q, want ABD", got)
	}
	if s.wrapPending {
		t.Fatal("wrapPending set while DECAWM disabled")
	}
}

func TestREPRepeatsPreviousGraphicCharacter(t *testing.T) {
	s := NewScreen(1, 10)
	p := NewParser(s)
	p.Feed([]byte("A\x1b[3b"))
	if got := s.LineText(0); got != "AAAA" {
		t.Fatalf("REP result = %q, want AAAA", got)
	}
}

func TestDECSCRestoresRenditionAndCharset(t *testing.T) {
	s := NewScreen(2, 10)
	p := NewParser(s)
	p.Feed([]byte("\x1b[31m\x1b(0\x1b7"))
	p.Feed([]byte("\x1b[0m\x1b(B\x1b[2;5H"))
	p.Feed([]byte("\x1b8q"))

	if got := s.cells[0][0].Rune; got != '─' {
		t.Fatalf("restored DEC charset rendered %q, want line drawing", got)
	}
	if got := s.cells[0][0].FG; got != lipgloss.Color("#800000") {
		t.Fatalf("restored SGR fg = %q, want dark red", got)
	}
}
