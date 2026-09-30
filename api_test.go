package portalis_test

import (
	"reflect"
	"testing"

	"github.com/starframe-dev/portalis"
)

func TestScreenMutableStateIsEncapsulated(t *testing.T) {
	typeOfScreen := reflect.TypeOf(portalis.Screen{})
	for _, field := range []string{"Rows", "Cols", "Cells", "Cursor", "CursorVisible", "CursorBlinkVisible"} {
		if _, ok := typeOfScreen.FieldByName(field); ok {
			t.Errorf("Screen.%s remains publicly mutable", field)
		}
	}
}

func TestScreenSnapshotsDoNotExposeInternalCells(t *testing.T) {
	screen := portalis.NewScreen(1, 2)
	screen.Put('x')

	cell, ok := screen.CellAt(0, 0)
	if !ok || cell.Rune != 'x' {
		t.Fatalf("CellAt(0, 0) = %+v, %v; want x, true", cell, ok)
	}
	cell.Rune = 'y'

	snapshot := screen.CellsSnapshot()
	snapshot[0][0].Rune = 'z'
	if got, ok := screen.CellAt(0, 0); !ok || got.Rune != 'x' {
		t.Fatalf("mutating a returned cell changed Screen: %+v, %v", got, ok)
	}
	if _, ok := screen.CellAt(-1, 0); ok {
		t.Fatal("CellAt accepted a negative row")
	}
	if rows, cols := screen.Rows(), screen.Cols(); rows != 1 || cols != 2 {
		t.Fatalf("dimensions = %dx%d, want 1x2", rows, cols)
	}
}

func TestStructuredOSCMetadataIsAvailableInPublicAPI(t *testing.T) {
	parser := portalis.NewParser(portalis.NewScreen(1, 20))
	var cwd portalis.WorkingDirectory
	var title string
	parser.SetCWDCallback(func(location portalis.WorkingDirectory) { cwd = location })
	parser.SetTitleCallback(func(value string) { title = value })
	parser.Feed([]byte("\x1b]7;file://build-host.example/work\x07\x1b]2;Build\x07"))
	if cwd.Host != "build-host.example" || cwd.Path != "/work" || cwd.Local {
		t.Fatalf("public OSC 7 metadata = %+v", cwd)
	}
	if title != "Build" {
		t.Fatalf("public OSC title = %q, want Build", title)
	}
}

func TestCommandHistorySnapshotIsCopiedAndExplicitlyHeuristic(t *testing.T) {
	emulator := portalis.NewEmulator("session", "Build", "sh", nil)
	if emulator.SessionID() != "session" || emulator.ChatName() != "Build" {
		t.Fatalf("immutable getters = %q/%q", emulator.SessionID(), emulator.ChatName())
	}
	emulator.SetCommandHistory([]string{"echo safe"})
	history := emulator.CommandHistorySnapshot()
	history[0] = "changed"
	if got := emulator.CommandHistorySnapshot(); len(got) != 1 || got[0] != "echo safe" {
		t.Fatalf("history snapshot mutation reached Emulator: %#v", got)
	}
}

func TestPtyStateSnapshotDoesNotExposeHandles(t *testing.T) {
	emulator := portalis.NewEmulator("state", "State", "sleep", []string{"10"})
	if err := emulator.StartSync(nil); err != nil {
		t.Fatal(err)
	}
	defer emulator.Close()

	state := emulator.PtyState()
	if !state.Running || state.PID <= 0 || state.Rows <= 0 || state.Cols <= 0 {
		t.Fatalf("PTY snapshot = %+v, want running process with dimensions", state)
	}
	if err := emulator.Close(); err != nil {
		t.Fatal(err)
	}
	if state := emulator.PtyState(); state.Running || state.PID != 0 {
		t.Fatalf("closed PTY snapshot = %+v, want empty state", state)
	}
}

func TestPtyAndEmulatorMutableStateIsEncapsulated(t *testing.T) {
	for _, test := range []struct {
		typeOf    reflect.Type
		forbidden []string
	}{
		{typeOf: reflect.TypeOf(portalis.Pty{}), forbidden: []string{"Output", "Errors", "ptmx", "cmd"}},
		{typeOf: reflect.TypeOf(portalis.Emulator{}), forbidden: []string{"Pty", "SessionID", "ChatName", "OnError", "OnExit"}},
	} {
		for _, field := range test.forbidden {
			if info, ok := test.typeOf.FieldByName(field); ok && info.PkgPath == "" {
				t.Errorf("%s.%s remains externally accessible", test.typeOf.Name(), field)
			}
		}
	}
	if _, ok := reflect.TypeOf((*portalis.Emulator)(nil)).MethodByName("Pty"); ok {
		t.Fatal("Emulator.Pty exposes the raw PTY")
	}
}
