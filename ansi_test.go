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

	if s.Cells[0][0].Rune != 'h' {
		t.Errorf("expected 'h', got %c", s.Cells[0][0].Rune)
	}
	if s.Cells[0][4].Rune != 'o' {
		t.Errorf("expected 'o', got %c", s.Cells[0][4].Rune)
	}
}

func TestParserCursor(t *testing.T) {
	s := NewScreen(5, 10)
	p := NewParser(s)
	p.Feed([]byte("\x1b[3;5Hab"))

	if s.Cells[2][4].Rune != 'a' {
		t.Errorf("expected 'a' at row 3 col 5, got %c", s.Cells[2][4].Rune)
	}
}

func TestParserColor(t *testing.T) {
	s := NewScreen(2, 10)
	p := NewParser(s)
	p.Feed([]byte("\x1b[31mred\x1b[0m"))

	if s.Cells[0][0].FG != lipgloss.Color("#800000") {
		t.Errorf("expected dark red fg, got %v", s.Cells[0][0].FG)
	}
}

func TestParserClear(t *testing.T) {
	s := NewScreen(3, 10)
	p := NewParser(s)
	p.Feed([]byte("hello"))
	p.Feed([]byte("\x1b[2J"))

	if s.Cells[0][0].Rune != 0 {
		t.Error("expected screen cleared")
	}
}

func TestParserNewline(t *testing.T) {
	s := NewScreen(3, 10)
	p := NewParser(s)
	p.Feed([]byte("hello\r\nworld"))

	if s.Cells[1][0].Rune != 'w' {
		t.Errorf("expected 'w' on second line, got %c", s.Cells[1][0].Rune)
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

	var cwd string
	p.SetCWDCallback(func(path string) {
		cwd = path
	})

	p.Feed([]byte("before\x1b]7;/Users/a/foo\x1b\\after"))

	if cwd != "/Users/a/foo" {
		t.Fatalf("cwd = %q, want /Users/a/foo", cwd)
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
	if !s.CursorVisible {
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
	if got := s.Cells[0][1].Rune; got != 'A' {
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

func TestOSC7PercentDecodedAndControlsRejected(t *testing.T) {
	if got := extractOSC7Path("file://localhost/tmp/a%20b"); got != "/tmp/a b" {
		t.Fatalf("decoded OSC 7 path = %q, want /tmp/a b", got)
	}
	if got := extractOSC7Path("file://localhost/tmp/bad%1Bname"); got != "" {
		t.Fatalf("OSC 7 control path accepted: %q", got)
	}
	if got := extractOSC7Path("/tmp/bad\nname"); got != "" {
		t.Fatalf("raw OSC 7 newline path accepted: %q", got)
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

	p.Feed([]byte("["))
	p.Feed([]byte("6n"))
	p.Feed([]byte("[5n"))

	want := []string{"[3;5R", "[0n"}
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

	p.Feed([]byte("[c[>c"))
	want := []string{"[?1;2c", "[>0;1;0c"}
	if len(replies) != len(want) || replies[0] != want[0] || replies[1] != want[1] {
		t.Fatalf("DA replies = %#v, want %#v", replies, want)
	}
}

func TestBracketedPasteMarkersInOutputDoNotEnterPasteState(t *testing.T) {
	s := NewScreen(2, 20)
	p := NewParser(s)
	p.Feed([]byte("[200~é[201~X"))
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
	p.Feed([]byte("[3;5Hhello[2J"))
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
	p.Feed([]byte("[2;5r[?6h[1;1H"))
	if row, col := s.CursorPos(); row != 1 || col != 0 {
		t.Fatalf("origin-mode CUP = %d,%d, want 1,0", row, col)
	}
	p.Feed([]byte("[?6l"))
	if row, col := s.CursorPos(); row != 0 || col != 0 {
		t.Fatalf("DECOM reset cursor = %d,%d, want 0,0", row, col)
	}
}

func TestAutoWrapModeCanBeDisabled(t *testing.T) {
	s := NewScreen(1, 3)
	p := NewParser(s)
	p.Feed([]byte("[?7lABCD"))
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
	p.Feed([]byte("A[3b"))
	if got := s.LineText(0); got != "AAAA" {
		t.Fatalf("REP result = %q, want AAAA", got)
	}
}

func TestDECSCRestoresRenditionAndCharset(t *testing.T) {
	s := NewScreen(2, 10)
	p := NewParser(s)
	p.Feed([]byte("[31m(07"))
	p.Feed([]byte("[0m(B[2;5H"))
	p.Feed([]byte("8q"))

	if got := s.Cells[0][0].Rune; got != '─' {
		t.Fatalf("restored DEC charset rendered %q, want line drawing", got)
	}
	if got := s.Cells[0][0].FG; got != lipgloss.Color("#800000") {
		t.Fatalf("restored SGR fg = %q, want dark red", got)
	}
}
