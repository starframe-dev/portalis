package portalis

import (
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestPtyOutputFeedsParserImmediatelyAndContinuesListener(t *testing.T) {
	screen := NewScreen(2, 20)
	emulator := NewEmulator("session", "Session", "/bin/sh", nil)
	emulator.screen = screen
	emulator.parser = NewParser(screen)
	emulator.pty = &Pty{}

	cmd := emulator.Update(PtyOutputMsg{SessionID: "session", Data: []byte("hello")})
	if cmd == nil {
		t.Fatal("PTY output did not continue the listener chain")
	}
	if got := screen.LineText(0); got != "hello" {
		t.Fatalf("rendered line = %q, want %q", got, "hello")
	}
}

func TestLegacyRenderTickDoesNotStartListener(t *testing.T) {
	emulator := NewEmulator("session", "Session", "/bin/sh", nil)
	if cmd := emulator.Update(RenderTickMsg{SessionID: "session"}); cmd != nil {
		t.Fatal("legacy render tick started a PTY listener")
	}
}

func TestListenAllowsOnlyOnePendingReader(t *testing.T) {
	output := make(chan []byte, 2)
	emulator := NewEmulator("session", "Session", "/bin/sh", nil)
	emulator.pty = &Pty{
		Output: output,
		Errors: make(chan error, 1),
	}

	first := emulator.Listen()
	if first == nil {
		t.Fatal("first Listen returned nil")
	}
	if second := emulator.Listen(); second != nil {
		t.Fatal("second Listen returned a command while the first reader was pending")
	}

	output <- []byte("first")
	message, ok := first().(PtyOutputMsg)
	if !ok {
		t.Fatalf("first Listen message type = %T, want PtyOutputMsg", message)
	}
	if string(message.Data) != "first" {
		t.Fatalf("first chunk = %q, want %q", message.Data, "first")
	}

	third := emulator.Listen()
	if third == nil {
		t.Fatal("Listen did not become available after the first reader completed")
	}
	output <- []byte("second")
	message, ok = third().(PtyOutputMsg)
	if !ok {
		t.Fatalf("third Listen message type = %T, want PtyOutputMsg", message)
	}
	if string(message.Data) != "second" {
		t.Fatalf("second chunk = %q, want %q", message.Data, "second")
	}
}

func TestAlreadyRunningReadyDoesNotStartListener(t *testing.T) {
	output := make(chan []byte, 1)
	emulator := NewEmulator("session", "Session", "/bin/sh", nil)
	emulator.pty = &Pty{
		Output: output,
		Errors: make(chan error, 1),
	}

	first := emulator.Listen()
	if first == nil {
		t.Fatal("first Listen returned nil")
	}
	if command := emulator.Update(PtyReadyMsg{SessionID: "session", AlreadyRunning: true}); command != nil {
		t.Fatal("AlreadyRunning ready message started a second listener")
	}

	output <- []byte("output")
	if _, ok := first().(PtyOutputMsg); !ok {
		t.Fatal("existing listener did not receive PTY output")
	}
}

func TestPtyOutputOSC7DoesNotDeadlock(t *testing.T) {
	emulator := NewEmulator("session", "Session", "/bin/sh", nil)
	emulator.mu.Lock()
	emulator.resetTerminalLocked()
	generation := emulator.listenerGeneration
	emulator.pty = &Pty{}
	emulator.mu.Unlock()

	var callbackCalls atomic.Int32
	emulator.OnCWDChange = func(path string) {
		if got := emulator.CWD(); got == path {
			callbackCalls.Add(1)
		}
	}

	finished := make(chan bool, 1)
	go func() {
		cmd := emulator.Update(PtyOutputMsg{
			SessionID:  "session",
			Generation: generation,
			Data:       []byte("\x1b]7;/tmp/project\x07prompt"),
		})
		finished <- cmd != nil
	}()

	select {
	case continued := <-finished:
		if !continued {
			t.Fatal("OSC 7 output stopped the listener chain")
		}
	case <-time.After(time.Second):
		t.Fatal("Emulator.Update deadlocked while handling OSC 7")
	}

	if got := emulator.CWD(); got != "/tmp/project" {
		t.Fatalf("cwd = %q, want /tmp/project", got)
	}
	if got := callbackCalls.Load(); got != 1 {
		t.Fatalf("CWD callback calls = %d, want 1", got)
	}
	if got := emulator.screen.LineText(0); got != "prompt" {
		t.Fatalf("rendered line = %q, want prompt", got)
	}

	emulator.Update(PtyOutputMsg{
		SessionID:  "session",
		Generation: generation,
		Data:       []byte("\x1b]7;/tmp/project\x07"),
	})
	if got := callbackCalls.Load(); got != 1 {
		t.Fatalf("unchanged CWD callback calls = %d, want 1", got)
	}
}

func TestStartEnvUsesDefensiveCopies(t *testing.T) {
	em := NewEmulator("session", "Session", "/bin/sh", nil)
	input := []string{"A=1"}
	em.SetStartEnv(input)
	input[0] = "MUTATED=1"
	if got := em.StartEnv(); len(got) != 1 || got[0] != "A=1" {
		t.Fatalf("stored env mutated through caller slice: %#v", got)
	}
	got := em.StartEnv()
	got[0] = "MUTATED=2"
	if again := em.StartEnv(); again[0] != "A=1" {
		t.Fatalf("stored env mutated through getter slice: %#v", again)
	}
}

func TestSetCommandHistoryUsesDefensiveCopy(t *testing.T) {
	em := NewEmulator("session", "Session", "/bin/sh", nil)
	history := []string{"echo safe"}
	em.SetCommandHistory(history)
	history[0] = "mutated"
	em.mu.RLock()
	got := em.commandHistory[0]
	em.mu.RUnlock()
	if got != "echo safe" {
		t.Fatalf("stored history mutated through caller slice: %q", got)
	}
}

func TestStalePtyOutputGenerationIsIgnored(t *testing.T) {
	em := NewEmulator("session", "Session", "/bin/sh", nil)
	em.mu.Lock()
	em.resetTerminalLocked()
	current := em.listenerGeneration
	em.mu.Unlock()

	em.Update(PtyOutputMsg{SessionID: "session", Generation: current + 1, Data: []byte("stale")})
	if got := em.screen.RenderLine(0); got[:5] == "stale" {
		t.Fatalf("stale PTY generation was rendered: %q", got)
	}
}

func TestCommandHistoryCallbackMayReenterEmulator(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	em := NewEmulator("session", "Session", "/bin/sh", nil)
	em.mu.Lock()
	em.resetTerminalLocked()
	em.pty = &Pty{ptmx: w, done: make(chan struct{})}
	em.screen.PutBytes([]byte("$ echo hi"))
	em.mu.Unlock()
	defer em.Close()

	done := make(chan struct{})
	em.OnCommandHistoryChanged = func(_ []string) {
		_ = em.CWD()
		close(done)
	}

	em.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("history callback deadlocked while re-entering Emulator")
	}
}

func TestStopCancelsQueuedStartCommand(t *testing.T) {
	em := NewEmulator("session", "Session", "/bin/sh", nil)
	cmd := em.Start()
	if cmd == nil {
		t.Fatal("Start returned nil command")
	}
	em.Stop()
	if msg := cmd(); msg != nil {
		t.Fatalf("cancelled Start returned %T, want nil", msg)
	}
	em.mu.RLock()
	defer em.mu.RUnlock()
	if em.pty != nil {
		t.Fatal("cancelled Start spawned a PTY")
	}
	if !em.stopped {
		t.Fatal("Stop state was cleared by cancelled Start")
	}
}

func TestResetTerminalClearsStaleCWD(t *testing.T) {
	em := NewEmulator("session", "Session", "/bin/sh", nil)
	em.SetInitialCWD("/initial")
	em.mu.Lock()
	em.cwd = "/old"
	em.resetTerminalLocked()
	em.mu.Unlock()
	if got := em.CWD(); got != "/initial" {
		t.Fatalf("CWD after reset = %q, want /initial fallback", got)
	}
}

func TestMouseEncodingUsesCorrectXtermCodes(t *testing.T) {
	wheel := tea.MouseMsg{X: 1, Y: 2, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp}
	if got, want := string(mouseToBytes(wheel, true)), "[<64;2;3M"; got != want {
		t.Fatalf("SGR wheel = %q, want %q", got, want)
	}

	left := tea.MouseMsg{X: 0, Y: 0, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft}
	got := mouseToBytes(left, false)
	want := []byte{0x1b, '[', 'M', 32, 33, 33}
	if string(got) != string(want) {
		t.Fatalf("X10 left = %v, want %v", got, want)
	}
}

func TestMouseTrackingRules(t *testing.T) {
	press := tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft}
	if shouldReportMouse(0, press) {
		t.Fatal("mouse press reported with tracking disabled")
	}
	if !shouldReportMouse(1000, press) {
		t.Fatal("mouse press not reported in mode 1000")
	}
	motionNone := tea.MouseMsg{Action: tea.MouseActionMotion, Button: tea.MouseButtonNone}
	if shouldReportMouse(1002, motionNone) {
		t.Fatal("buttonless motion reported in mode 1002")
	}
	if !shouldReportMouse(1003, motionNone) {
		t.Fatal("buttonless motion not reported in mode 1003")
	}
}

func TestNewEmulatorCopiesArgs(t *testing.T) {
	args := []string{"-c", "echo safe"}
	em := NewEmulator("session", "Session", "/bin/sh", args)
	args[1] = "mutated"
	if got := em.args[1]; got != "echo safe" {
		t.Fatalf("emulator args mutated through caller slice: %q", got)
	}
}

func TestGenerationZeroDoesNotBypassRestartGuard(t *testing.T) {
	em := NewEmulator("session", "Session", "/bin/sh", nil)
	em.mu.Lock()
	em.resetTerminalLocked()
	em.mu.Unlock()

	em.Update(PtyOutputMsg{SessionID: "session", Generation: 0, Data: []byte("stale")})
	if got := em.screen.LineText(0); got != "" {
		t.Fatalf("generation-zero stale output was accepted: %q", got)
	}
}

func TestLegacyMouseReleaseUsesReleaseButtonCode(t *testing.T) {
	msg := tea.MouseMsg{X: 1, Y: 2, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft}
	got := mouseToBytes(msg, false)
	want := []byte{0x1b, '[', 'M', 35, 34, 35}
	if string(got) != string(want) {
		t.Fatalf("legacy release = %v, want %v", got, want)
	}
}

func TestFocusReportingWritesDECSequence(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	em := NewEmulator("session", "Session", "/bin/sh", nil)
	em.mu.Lock()
	em.screen = NewScreen(2, 10)
	em.screen.focusReporting = true
	em.pty = &Pty{ptmx: w}
	em.mu.Unlock()

	cmd := em.Focus()
	if cmd == nil {
		t.Fatal("Focus returned nil while ?1004 reporting enabled")
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("focus command returned %T, want nil", msg)
	}

	buf := make([]byte, 3)
	if _, err := r.Read(buf); err != nil {
		t.Fatal(err)
	}
	if got := string(buf); got != "[I" {
		t.Fatalf("focus report = %q, want ESC[I", got)
	}
}

func TestErrorMessagesReachOnError(t *testing.T) {
	em := NewEmulator("session", "Session", "/bin/sh", nil)
	var calls atomic.Int32
	em.OnError = func(err error) {
		if err != nil {
			calls.Add(1)
		}
	}
	em.Update(ClipboardErrorMsg{Err: errors.New("clipboard")})
	em.Update(PtyErrorMsg{Err: errors.New("pty")})
	if got := calls.Load(); got != 2 {
		t.Fatalf("OnError calls = %d, want 2", got)
	}
}

func TestStaleReadyGenerationDoesNotStartListener(t *testing.T) {
	em := NewEmulator("session", "Session", "/bin/sh", nil)
	em.mu.Lock()
	em.resetTerminalLocked()
	current := em.listenerGeneration
	em.pty = &Pty{Output: make(chan []byte), Errors: make(chan error)}
	em.mu.Unlock()

	if cmd := em.Update(PtyReadyMsg{SessionID: "session", Generation: current - 1}); cmd != nil {
		t.Fatal("stale PtyReadyMsg started a listener")
	}
}

func TestLocalDragReleaseWinsAfterShiftIsReleased(t *testing.T) {
	em := NewEmulator("session", "Session", "/bin/sh", nil)
	em.mu.Lock()
	em.screen = NewScreen(1, 8)
	em.screen.PutBytes([]byte("selection"))
	em.screen.mouseMode1000 = true
	em.screen.StartSelection(0, 0)
	em.screen.ExtendSelection(0, 3)
	em.dragSelecting = true
	em.mu.Unlock()

	_ = em.handleMouse(tea.MouseMsg{
		X:      3,
		Y:      0,
		Action: tea.MouseActionRelease,
		Button: tea.MouseButtonLeft,
	})
	em.mu.RLock()
	defer em.mu.RUnlock()
	if em.dragSelecting {
		t.Fatal("local drag remained active after release without Shift")
	}
	if em.screen.selectionActive {
		t.Fatal("selection remained active after local release")
	}
}

func TestCloseRemovesTrackedClipboardTempFiles(t *testing.T) {
	file, err := os.CreateTemp("", "portalis-cleanup-*")
	if err != nil {
		t.Fatal(err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	em := NewEmulator("session", "Session", "/bin/sh", nil)
	em.mu.Lock()
	em.tempFiles = append(em.tempFiles, path)
	em.mu.Unlock()
	em.Close()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("tracked temp file still exists after Close: %v", err)
	}
}
