package portalis

import (
	"reflect"
	"testing"
)

func FuzzParserFeed(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("hello"),
		[]byte("\x1b[31mred\x1b[0m"),
		[]byte("\x1b]7;file://localhost/tmp/a%20b\x07"),
		[]byte("emoji: 🎉 你好 e\u0301"),
		[]byte("\x1b[?1002;1006h"),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		screen := NewScreen(24, 80)
		parser := NewParser(screen)

		// Exercise both whole-buffer and fragmented delivery because PTY read
		// boundaries may split UTF-8 and escape sequences arbitrarily.
		parser.Feed(data)
		if len(data) > 1 {
			screen2 := NewScreen(24, 80)
			parser2 := NewParser(screen2)
			mid := len(data) / 2
			parser2.Feed(data[:mid])
			parser2.Feed(data[mid:])

			wholeRender := screen.Render()
			splitRender := screen2.Render()
			if wholeRender != splitRender ||
				screen.Cursor != screen2.Cursor ||
				parser.state != parser2.state ||
				parser.buf.String() != parser2.buf.String() ||
				!reflect.DeepEqual(parser.utf8Buf, parser2.utf8Buf) {
				t.Fatalf("chunk-boundary semantic mismatch")
			}
			return
		}
		_ = screen.Render()
	})
}
