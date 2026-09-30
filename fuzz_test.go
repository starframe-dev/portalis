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
		f.Add(seed, []byte{1, 3, 2})
	}

	f.Fuzz(func(t *testing.T, data, chunkPattern []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		if len(chunkPattern) > 128 {
			chunkPattern = chunkPattern[:128]
		}
		wholeScreen := NewScreen(24, 80)
		wholeParser := NewParser(wholeScreen)
		wholeParser.Feed(data)

		splitScreen := NewScreen(24, 80)
		splitParser := NewParser(splitScreen)
		if len(chunkPattern) == 0 {
			chunkPattern = []byte{1}
		}
		for offset, patternIndex := 0, 0; offset < len(data); patternIndex++ {
			size := int(chunkPattern[patternIndex%len(chunkPattern)]) + 1
			if size > len(data)-offset {
				size = len(data) - offset
			}
			splitParser.Feed(data[offset : offset+size])
			offset += size
		}

		if wholeScreen.rows != splitScreen.rows ||
			wholeScreen.cols != splitScreen.cols ||
			!reflect.DeepEqual(wholeScreen.cells, splitScreen.cells) ||
			!reflect.DeepEqual(wholeScreen.scrollback, splitScreen.scrollback) ||
			!reflect.DeepEqual(wholeScreen.savedCells, splitScreen.savedCells) ||
			wholeScreen.cursor != splitScreen.cursor ||
			wholeScreen.savedCursor != splitScreen.savedCursor ||
			wholeScreen.savedOriginMode != splitScreen.savedOriginMode ||
			wholeScreen.savedAutoWrap != splitScreen.savedAutoWrap ||
			wholeScreen.savedWrapPending != splitScreen.savedWrapPending ||
			wholeScreen.altSavedCursor != splitScreen.altSavedCursor ||
			wholeScreen.altSavedScrollTop != splitScreen.altSavedScrollTop ||
			wholeScreen.altSavedScrollBottom != splitScreen.altSavedScrollBottom ||
			wholeScreen.altSavedOriginMode != splitScreen.altSavedOriginMode ||
			wholeScreen.altSavedAutoWrap != splitScreen.altSavedAutoWrap ||
			wholeScreen.altSavedWrapPending != splitScreen.altSavedWrapPending ||
			wholeScreen.wrapPending != splitScreen.wrapPending ||
			wholeScreen.scrollTop != splitScreen.scrollTop ||
			wholeScreen.scrollBottom != splitScreen.scrollBottom ||
			wholeScreen.scrollbackCells != splitScreen.scrollbackCells ||
			wholeScreen.scrollbackLimit != splitScreen.scrollbackLimit ||
			wholeScreen.viewOffset != splitScreen.viewOffset ||
			wholeScreen.originMode != splitScreen.originMode ||
			wholeScreen.autoWrap != splitScreen.autoWrap ||
			wholeScreen.insertMode != splitScreen.insertMode ||
			!reflect.DeepEqual(wholeScreen.tabStops, splitScreen.tabStops) ||
			wholeScreen.applicationCursor != splitScreen.applicationCursor ||
			wholeScreen.bracketedPaste != splitScreen.bracketedPaste ||
			wholeScreen.mouseMode1000 != splitScreen.mouseMode1000 ||
			wholeScreen.mouseMode1002 != splitScreen.mouseMode1002 ||
			wholeScreen.mouseMode1003 != splitScreen.mouseMode1003 ||
			wholeScreen.mouseSGR != splitScreen.mouseSGR ||
			wholeScreen.focusReporting != splitScreen.focusReporting ||
			wholeScreen.cursorVisible != splitScreen.cursorVisible ||
			wholeScreen.cursorBlinkVisible != splitScreen.cursorBlinkVisible ||
			wholeScreen.syncActive != splitScreen.syncActive ||
			wholeScreen.renderDirty != splitScreen.renderDirty ||
			wholeScreen.selectionActive != splitScreen.selectionActive ||
			wholeScreen.selStartRow != splitScreen.selStartRow ||
			wholeScreen.selStartCol != splitScreen.selStartCol ||
			wholeScreen.selEndRow != splitScreen.selEndRow ||
			wholeScreen.selEndCol != splitScreen.selEndCol ||
			wholeScreen.altScreen != splitScreen.altScreen ||
			wholeParser.state != splitParser.state ||
			wholeParser.buf.String() != splitParser.buf.String() ||
			wholeParser.escapeIntermediate != splitParser.escapeIntermediate ||
			!reflect.DeepEqual(wholeParser.utf8Buf, splitParser.utf8Buf) ||
			wholeParser.g0LineDrawing != splitParser.g0LineDrawing ||
			wholeParser.g1LineDrawing != splitParser.g1LineDrawing ||
			wholeParser.useG1 != splitParser.useG1 ||
			wholeParser.savedG0LineDrawing != splitParser.savedG0LineDrawing ||
			wholeParser.savedG1LineDrawing != splitParser.savedG1LineDrawing ||
			wholeParser.savedUseG1 != splitParser.savedUseG1 ||
			wholeParser.altSavedG0LineDrawing != splitParser.altSavedG0LineDrawing ||
			wholeParser.altSavedG1LineDrawing != splitParser.altSavedG1LineDrawing ||
			wholeParser.altSavedUseG1 != splitParser.altSavedUseG1 ||
			wholeParser.altCharsetSaved != splitParser.altCharsetSaved ||
			wholeParser.lastCWD != splitParser.lastCWD ||
			wholeParser.lastTitle != splitParser.lastTitle ||
			wholeScreen.Render() != splitScreen.Render() {
			t.Fatalf("arbitrary chunk-boundary semantic mismatch")
		}
	})
}
