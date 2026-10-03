package portalis

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	creackpty "github.com/creack/pty"
)

func TestStartResizeUsesLatestDimensions(t *testing.T) {
	emulator := NewEmulator("session", "Session", "/bin/sh", nil)
	emulator.screen = NewScreen(defaultTerminalRows, defaultTerminalCols)
	emulator.listenerGeneration = 1
	pty := &Pty{ptmx: &os.File{}}
	firstResizeStarted := make(chan struct{})
	releaseFirstResize := make(chan struct{})
	var applied []creackpty.Winsize
	pty.setSize = func(_ *os.File, size *creackpty.Winsize) error {
		applied = append(applied, *size)
		if len(applied) == 1 {
			close(firstResizeStarted)
			<-releaseFirstResize
		}
		return nil
	}
	emulator.pty = pty
	generation := emulator.listenerGeneration

	startResize := make(chan error, 1)
	go func() {
		_, err := emulator.resizeAttachedPTY(pty, generation)
		startResize <- err
	}()
	<-firstResizeStarted

	// This is the locked request update used by ResizeMsg. Keep the first PTY
	// ioctl blocked so Start must detect the newer generation before returning.
	emulator.mu.Lock()
	if err := emulator.requestTerminalSizeLocked(40, 120); err != nil {
		emulator.mu.Unlock()
		close(releaseFirstResize)
		t.Fatal(err)
	}
	emulator.mu.Unlock()
	if emulator.screen.rows != defaultTerminalRows || emulator.screen.cols != defaultTerminalCols {
		close(releaseFirstResize)
		t.Fatalf("Screen changed before TIOCSWINSZ succeeded: %dx%d", emulator.screen.rows, emulator.screen.cols)
	}
	close(releaseFirstResize)

	if err := <-startResize; err != nil {
		t.Fatalf("start resize: %v", err)
	}
	if pty.lastRows != 40 || pty.lastCols != 120 {
		t.Fatalf("PTY size = %dx%d, want latest 40x120", pty.lastRows, pty.lastCols)
	}
	if emulator.screen.rows != 40 || emulator.screen.cols != 120 {
		t.Fatalf("Screen size = %dx%d, want 40x120", emulator.screen.rows, emulator.screen.cols)
	}
	if emulator.height != 40 || emulator.width != 120 {
		t.Fatalf("applied emulator size = %dx%d, want 40x120", emulator.height, emulator.width)
	}
	if len(applied) != 2 || applied[0].Rows != 24 || applied[0].Cols != 80 || applied[1].Rows != 40 || applied[1].Cols != 120 {
		t.Fatalf("applied PTY sizes = %#v, want [24x80 40x120]", applied)
	}
}

func TestResizeRetriesLatestAfterStaleFailure(t *testing.T) {
	emulator := NewEmulator("session", "Session", "/bin/sh", nil)
	emulator.screen = NewScreen(defaultTerminalRows, defaultTerminalCols)
	emulator.listenerGeneration = 1
	if err := emulator.requestTerminalSizeLocked(25, defaultTerminalCols); err != nil {
		t.Fatal(err)
	}
	firstResizeStarted := make(chan struct{})
	releaseFirstResize := make(chan struct{})
	var calls int
	pty := &Pty{ptmx: &os.File{}, lastRows: defaultTerminalRows, lastCols: defaultTerminalCols}
	pty.setSize = func(_ *os.File, _ *creackpty.Winsize) error {
		calls++
		if calls == 1 {
			close(firstResizeStarted)
			<-releaseFirstResize
			return errors.New("stale ioctl failure")
		}
		return nil
	}
	emulator.pty = pty
	generation := emulator.listenerGeneration
	resizeDone := make(chan error, 1)
	go func() {
		_, err := emulator.resizeAttachedPTY(pty, generation)
		resizeDone <- err
	}()
	<-firstResizeStarted

	emulator.mu.Lock()
	if err := emulator.requestTerminalSizeLocked(40, 120); err != nil {
		emulator.mu.Unlock()
		close(releaseFirstResize)
		t.Fatal(err)
	}
	emulator.mu.Unlock()
	close(releaseFirstResize)

	if err := <-resizeDone; err != nil {
		t.Fatalf("resize returned stale error: %v", err)
	}
	if calls != 2 || pty.lastRows != 40 || pty.lastCols != 120 {
		t.Fatalf("resize calls=%d PTY=%dx%d, want 2 calls and latest 40x120", calls, pty.lastRows, pty.lastCols)
	}
	if emulator.screen.rows != 40 || emulator.screen.cols != 120 {
		t.Fatalf("Screen size = %dx%d, want latest 40x120", emulator.screen.rows, emulator.screen.cols)
	}
}

func TestPanelResizeFailureKeepsPTYAndAppliedSize(t *testing.T) {
	emulator := NewEmulator("session", "Session", "/bin/sh", nil)
	screen := NewScreen(2, 3)
	emulator.screen = screen
	emulator.height, emulator.width = 2, 3
	emulator.requestedHeight, emulator.requestedWidth = 2, 3
	emulator.listenerGeneration = 1
	ioctlErr := errors.New("TIOCSWINSZ failed")
	pty := &Pty{
		ptmx:     &os.File{},
		lastRows: 2,
		lastCols: 3,
		setSize: func(_ *os.File, _ *creackpty.Winsize) error {
			return ioctlErr
		},
	}
	emulator.pty = pty

	cmd := emulator.Update(ResizeMsg{Width: 4, Height: 3})
	if cmd == nil {
		t.Fatal("resize failure did not produce a nonfatal warning")
	}
	msg, ok := cmd().(PtyWarningMsg)
	if !ok || !errors.Is(msg.Err, ioctlErr) {
		t.Fatalf("resize error message = %#v, want PtyWarningMsg wrapping ioctl error", msg)
	}
	var reported error
	emulator.SetOnError(func(err error) { reported = err })
	if listener := emulator.Update(msg); listener == nil || !errors.Is(reported, ioctlErr) {
		t.Fatalf("resize error was not reported non-fatally: listener=%v reported=%v", listener != nil, reported)
	}
	if emulator.pty != pty || emulator.stopped {
		t.Fatal("resize error detached or stopped the PTY")
	}
	if screen.rows != 2 || screen.cols != 3 || emulator.height != 2 || emulator.width != 3 {
		t.Fatalf("failed resize changed applied size: Screen=%dx%d emulator=%dx%d", screen.rows, screen.cols, emulator.height, emulator.width)
	}
	if emulator.requestedHeight != 3 || emulator.requestedWidth != 4 {
		t.Fatalf("requested size=%dx%d, want 3x4", emulator.requestedHeight, emulator.requestedWidth)
	}
	if pty.lastRows != 2 || pty.lastCols != 3 {
		t.Fatalf("PTY size changed after failed ioctl to %dx%d", pty.lastRows, pty.lastCols)
	}
}

func TestInitialPTYHasSaneWinsize(t *testing.T) {
	pty, err := Spawn("/bin/sh", []string{"-c", "stty size"})
	if err != nil {
		t.Fatal(err)
	}
	defer pty.Close()

	var output strings.Builder
	for {
		msg := pty.Listen("session")()
		switch msg := msg.(type) {
		case PtyOutputMsg:
			output.Write(msg.Data)
		case PtyExitMsg:
			if !strings.Contains(output.String(), "24 80") {
				t.Fatalf("child observed initial PTY size %q, want 24 80", output.String())
			}
			return
		default:
			t.Fatalf("unexpected PTY message %T", msg)
		}
	}
}

func TestResizeWithoutPTYAppliesScreenImmediately(t *testing.T) {
	emulator := NewEmulator("session", "Session", "/bin/sh", nil)
	emulator.screen = NewScreen(2, 3)
	emulator.height, emulator.width = 2, 3
	emulator.requestedHeight, emulator.requestedWidth = 2, 3

	if cmd := emulator.Update(ResizeMsg{Width: 4, Height: 3}); cmd != nil {
		t.Fatalf("resize without PTY returned command %T", cmd)
	}
	if emulator.screen.rows != 3 || emulator.screen.cols != 4 || emulator.height != 3 || emulator.width != 4 {
		t.Fatalf("screen/applied size = Screen %dx%d, size %dx%d; want 3x4", emulator.screen.rows, emulator.screen.cols, emulator.height, emulator.width)
	}
	if emulator.requestedHeight != 3 || emulator.requestedWidth != 4 {
		t.Fatalf("requested size = %dx%d, want 3x4", emulator.requestedHeight, emulator.requestedWidth)
	}
}

func TestPtyReadyReportsResizeErrorAndStartsListener(t *testing.T) {
	emulator := NewEmulator("session", "Session", "/bin/sh", nil)
	emulator.listenerGeneration = 1
	emulator.pty = &Pty{
		output:   make(chan []byte),
		errors:   make(chan error),
		warnings: make(chan error),
	}
	resizeErr := errors.New("resize warning")
	var reported error
	emulator.SetOnError(func(err error) { reported = err })

	cmd := emulator.Update(PtyReadyMsg{SessionID: "session", Generation: 1, ResizeErr: resizeErr})
	if cmd == nil || reported != resizeErr {
		t.Fatalf("ready resize error handling: listener=%v reported=%v", cmd != nil, reported)
	}
}

func TestScrollbackZeroBeforeStartDisablesLimit(t *testing.T) {
	emulator := NewEmulator("session", "Session", "/bin/sh", []string{"-c", "sleep 5"})
	emulator.SetScrollbackLimit(0)
	if err := emulator.StartSync(nil); err != nil {
		t.Fatal(err)
	}
	defer emulator.Close()

	emulator.mu.RLock()
	limit := emulator.screen.scrollbackLimit
	emulator.mu.RUnlock()
	if limit != 0 {
		t.Fatalf("pre-start scrollback limit = %d, want 0 (unlimited)", limit)
	}
}

func TestPanelResizeRejectsInvalidDimensionsBeforeMutation(t *testing.T) {
	screen := NewScreen(2, 3)
	resizeCalls := 0
	pty := &Pty{
		ptmx: &os.File{},
		setSize: func(_ *os.File, _ *creackpty.Winsize) error {
			resizeCalls++
			return nil
		},
	}
	emulator := NewEmulator("session", "Session", "/bin/sh", nil)
	emulator.screen = screen
	emulator.pty = pty

	for _, size := range []ResizeMsg{
		{Width: 65536, Height: 1},
		{Width: 1000000, Height: 1000000},
	} {
		if cmd := emulator.Update(size); cmd != nil {
			t.Fatalf("invalid resize returned command %T", cmd)
		}
		if screen.rows != 2 || screen.cols != 3 {
			t.Fatalf("invalid resize changed screen to %dx%d", screen.rows, screen.cols)
		}
	}
	if resizeCalls != 0 {
		t.Fatalf("invalid resize reached PTY %d times, want 0", resizeCalls)
	}
	if emulator.requestedWidth != defaultTerminalCols || emulator.requestedHeight != defaultTerminalRows {
		t.Fatalf("invalid resize changed requested size to %dx%d", emulator.requestedHeight, emulator.requestedWidth)
	}
}

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
		output: output,
		errors: make(chan error, 1),
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
		output: output,
		errors: make(chan error, 1),
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

func TestInteractiveKeyEnqueueDoesNotWaitForBlockedPTYWrite(t *testing.T) {
	pty, err := Spawn("/bin/sh", []string{"-c", "sleep 5"})
	if err != nil {
		t.Fatal(err)
	}
	defer pty.Close()

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- pty.Write(bytes.Repeat([]byte{'x'}, 16<<20))
	}()
	deadline := time.Now().Add(time.Second)
	for {
		pty.writeBudget.mu.Lock()
		queuedBytes := pty.writeBudget.used
		pty.writeBudget.mu.Unlock()
		if queuedBytes > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("large PTY write was not admitted")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(25 * time.Millisecond)
	select {
	case err := <-writeDone:
		t.Fatalf("large PTY write unexpectedly completed before exercising backpressure: %v", err)
	default:
	}

	em := NewEmulator("session", "Session", "/bin/sh", nil)
	em.mu.Lock()
	em.pty = pty
	em.listenerGeneration = 0
	em.screen = NewScreen(24, 80)
	em.mu.Unlock()

	started := time.Now()
	cmd := em.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("key update blocked for %s while PTY writer was backpressured", elapsed)
	}
	if cmd == nil {
		t.Fatal("interactive write did not return a completion command")
	}

	if err := pty.Close(); err != nil {
		t.Fatalf("close blocked PTY: %v", err)
	}
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("blocked PTY write did not unblock during close")
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("completion command after close = %#v, want nil", msg)
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
	emulator.SetOnCWDChange(func(cwd WorkingDirectory) {
		if got, ok := emulator.CurrentWorkingDirectory(); ok && got == cwd {
			callbackCalls.Add(1)
		}
	})

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

func TestSetCommandHistoryKeepsNewestLimit(t *testing.T) {
	history := make([]string, maxCommandHistory+1)
	history[0] = "oldest"
	history[1] = "first retained"
	history[len(history)-1] = "newest"
	em := NewEmulator("session", "Session", "/bin/sh", nil)
	em.SetCommandHistory(history)

	em.mu.RLock()
	defer em.mu.RUnlock()
	if len(em.commandHistory) != maxCommandHistory {
		t.Fatalf("history size = %d, want %d", len(em.commandHistory), maxCommandHistory)
	}
	if em.commandHistory[0] != "first retained" || em.commandHistory[len(em.commandHistory)-1] != "newest" {
		t.Fatalf("restored history did not retain newest entries: first=%q last=%q", em.commandHistory[0], em.commandHistory[len(em.commandHistory)-1])
	}
}

func TestEmulatorSurfacesTraceWarningAndContinuesListening(t *testing.T) {
	em := NewEmulator("session", "Session", "/bin/sh", nil)
	em.mu.Lock()
	em.resetTerminalLocked()
	generation := em.listenerGeneration
	em.pty = &Pty{output: make(chan []byte), errors: make(chan error), warnings: make(chan error)}
	em.mu.Unlock()

	var warnings atomic.Int32
	em.SetOnError(func(error) { warnings.Add(1) })
	cmd := em.Update(PtyWarningMsg{
		SessionID:  "session",
		Generation: generation,
		Err:        errors.New("trace rotation failed"),
	})
	if warnings.Load() != 1 {
		t.Fatalf("OnError calls = %d, want one trace warning", warnings.Load())
	}
	if cmd == nil {
		t.Fatal("trace warning stopped the PTY listener chain")
	}
}

func TestEmulatorPreservesStructuredOSCMetadata(t *testing.T) {
	em := NewEmulator("session", "Session", "/bin/sh", nil)
	em.mu.Lock()
	em.resetTerminalLocked()
	generation := em.listenerGeneration
	em.pty = &Pty{}
	em.mu.Unlock()

	cwdCalls := make(chan WorkingDirectory, 1)
	titleCalls := make(chan string, 1)
	em.SetOnCWDChange(func(cwd WorkingDirectory) {
		if current, ok := em.CurrentWorkingDirectory(); ok && current == cwd {
			cwdCalls <- cwd
		}
	})
	em.SetOnTitleChange(func(title string) {
		if em.Title() == title {
			titleCalls <- title
		}
	})
	em.Update(PtyOutputMsg{
		SessionID:  "session",
		Generation: generation,
		Data:       []byte("\x1b]7;file://build-host.example/work/project\x07\x1b]2;Build\x07"),
	})

	select {
	case cwd := <-cwdCalls:
		if cwd.Host != "build-host.example" || cwd.Local || cwd.Path != "/work/project" {
			t.Fatalf("OSC 7 callback = %+v", cwd)
		}
	case <-time.After(time.Second):
		t.Fatal("OSC 7 callback did not run")
	}
	select {
	case title := <-titleCalls:
		if title != "Build" {
			t.Fatalf("title callback = %q, want Build", title)
		}
	case <-time.After(time.Second):
		t.Fatal("OSC title callback did not run")
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
	em.SetOnCommandHistoryChanged(func(_ []string) {
		_ = em.CWD()
		close(done)
	})

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
	em.cwd = WorkingDirectory{Path: "/old", Local: true}
	em.cwdSet = true
	em.resetTerminalLocked()
	em.mu.Unlock()
	if got := em.CWD(); got != "/initial" {
		t.Fatalf("CWD after reset = %q, want /initial fallback", got)
	}
}

func TestMouseEncodingUsesCorrectXtermCodes(t *testing.T) {
	wheel := tea.MouseMsg{X: 1, Y: 2, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp}
	if got, want := string(mouseToBytes(wheel, true)), "\x1b[<64;2;3M"; got != want {
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
	if got := string(buf); got != "\x1b[I" {
		t.Fatalf("focus report = %q, want ESC[I", got)
	}
}

func TestErrorMessagesReachOnError(t *testing.T) {
	em := NewEmulator("session", "Session", "/bin/sh", nil)
	var calls atomic.Int32
	em.SetOnError(func(err error) {
		if err != nil {
			calls.Add(1)
		}
	})
	em.Update(ClipboardErrorMsg{Err: errors.New("clipboard")})
	em.Update(PtyErrorMsg{Err: errors.New("pty")})
	if got := calls.Load(); got != 2 {
		t.Fatalf("OnError calls = %d, want 2", got)
	}
}

func TestOnExitCallbackReceivesProcessStatusOutsideLock(t *testing.T) {
	em := NewEmulator("session", "Session", "/bin/sh", nil)
	store, err := newClipboardTempStore(DefaultClipboardTempPolicy())
	if err != nil {
		t.Fatal(err)
	}
	dir := store.directory()
	t.Cleanup(func() { _ = store.close() })
	em.mu.Lock()
	em.resetTerminalLocked()
	generation := em.listenerGeneration
	em.pty = &Pty{}
	em.clipboardTempStore = store
	em.mu.Unlock()

	called := make(chan PtyExitMsg, 1)
	em.SetOnExit(func(msg PtyExitMsg) {
		_ = em.CWD()
		called <- msg
	})
	em.Update(PtyExitMsg{
		SessionID:     em.SessionID(),
		Generation:    generation,
		ProcessExited: true,
		ExitCode:      17,
	})

	select {
	case got := <-called:
		if got.ExitCode != 17 || !got.ProcessExited {
			t.Fatalf("OnExit status = %#v, want exited with code 17", got)
		}
	case <-time.After(time.Second):
		t.Fatal("OnExit callback was not invoked")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("clipboard directory remains after child exit: %v", err)
	}
}

func waitForEmulatorExit(t *testing.T, em *Emulator) PtyExitMsg {
	t.Helper()
	cmd := em.Listen()
	for cmd != nil {
		result := make(chan tea.Msg, 1)
		go func(next tea.Cmd) { result <- next() }(cmd)
		select {
		case msg := <-result:
			if exit, ok := msg.(PtyExitMsg); ok {
				em.Update(exit)
				return exit
			}
			cmd = em.Update(msg)
		case <-time.After(10 * time.Second):
			_ = em.Close()
			<-result
			t.Fatal("timed out waiting for child exit")
		}
	}
	t.Fatal("Emulator.Listen returned no command for running child")
	return PtyExitMsg{}
}

func startExitTestEmulator(t *testing.T, command string, args ...string) *Emulator {
	t.Helper()
	em := NewEmulator("session", "Session", command, args)
	if err := em.StartSync(nil); err != nil {
		t.Fatalf("start child: %v", err)
	}
	t.Cleanup(func() { _ = em.Close() })
	return em
}

func TestOnExitNaturalExitExactlyOnce(t *testing.T) {
	em := startExitTestEmulator(t, "/bin/sh", "-c", "exit 0")
	var calls atomic.Int32
	em.SetOnExit(func(PtyExitMsg) { calls.Add(1) })
	exit := waitForEmulatorExit(t, em)
	if !exit.ProcessExited || exit.ExitCode != 0 || exit.Signal != nil {
		t.Fatalf("natural exit status = %+v, want code 0", exit)
	}
	em.Update(exit)
	if got := calls.Load(); got != 1 {
		t.Fatalf("natural OnExit calls = %d, want exactly once", got)
	}
}

func TestOnExitNonZeroExitExactlyOnce(t *testing.T) {
	em := startExitTestEmulator(t, "/bin/sh", "-c", "exit 23")
	var calls atomic.Int32
	em.SetOnExit(func(PtyExitMsg) { calls.Add(1) })
	exit := waitForEmulatorExit(t, em)
	if !exit.ProcessExited || exit.ExitCode != 23 || exit.Signal != nil {
		t.Fatalf("non-zero exit status = %+v, want code 23", exit)
	}
	em.Update(exit)
	if got := calls.Load(); got != 1 {
		t.Fatalf("non-zero OnExit calls = %d, want exactly once", got)
	}
}

func TestOnExitSignalExitExactlyOnce(t *testing.T) {
	em := startExitTestEmulator(t, "/bin/sleep", "30")
	var calls atomic.Int32
	em.SetOnExit(func(PtyExitMsg) { calls.Add(1) })
	state := em.PtyState()
	process, err := os.FindProcess(state.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal child: %v", err)
	}
	exit := waitForEmulatorExit(t, em)
	if !exit.ProcessExited || exit.Signal != syscall.SIGTERM {
		t.Fatalf("signal exit status = %+v, want SIGTERM", exit)
	}
	em.Update(exit)
	if got := calls.Load(); got != 1 {
		t.Fatalf("signal OnExit calls = %d, want exactly once", got)
	}
}

func TestOnExitStopSemantics(t *testing.T) {
	em := startExitTestEmulator(t, "/bin/sleep", "30")
	var calls atomic.Int32
	em.SetOnExit(func(PtyExitMsg) { calls.Add(1) })
	staleListen := em.Listen()
	if staleListen == nil {
		t.Fatal("Listen returned nil for running child")
	}
	if err := em.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	msg := staleListen()
	exit, ok := msg.(PtyExitMsg)
	if !ok {
		t.Fatalf("stale listener message = %T, want PtyExitMsg", msg)
	}
	em.Update(exit)
	if got := calls.Load(); got != 0 {
		t.Fatalf("Stop invoked OnExit %d times, want none", got)
	}
}

func TestOnExitCloseSemantics(t *testing.T) {
	em := startExitTestEmulator(t, "/bin/sleep", "30")
	var calls atomic.Int32
	em.SetOnExit(func(PtyExitMsg) { calls.Add(1) })
	staleListen := em.Listen()
	if staleListen == nil {
		t.Fatal("Listen returned nil for running child")
	}
	if err := em.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	msg := staleListen()
	exit, ok := msg.(PtyExitMsg)
	if !ok {
		t.Fatalf("stale listener message = %T, want PtyExitMsg", msg)
	}
	em.Update(exit)
	if got := calls.Load(); got != 0 {
		t.Fatalf("Close invoked OnExit %d times, want none", got)
	}
}

func TestStaleReadyGenerationDoesNotStartListener(t *testing.T) {
	em := NewEmulator("session", "Session", "/bin/sh", nil)
	em.mu.Lock()
	em.resetTerminalLocked()
	current := em.listenerGeneration
	em.pty = &Pty{output: make(chan []byte), errors: make(chan error)}
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

func TestCloseRemovesPrivateClipboardDirectory(t *testing.T) {
	store, err := newClipboardTempStore(DefaultClipboardTempPolicy())
	if err != nil {
		t.Fatal(err)
	}
	dir := store.directory()
	path, err := os.CreateTemp(dir, "clipboard-test-*")
	if err != nil {
		t.Fatal(err)
	}
	filePath := path.Name()
	if _, err := path.Write([]byte("clipboard")); err != nil {
		t.Fatal(err)
	}
	if err := path.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.register(filePath); err != nil {
		t.Fatal(err)
	}

	em := NewEmulator("session", "Session", "/bin/sh", nil)
	em.mu.Lock()
	em.clipboardTempStore = store
	em.mu.Unlock()
	if err := em.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("private clipboard directory remains after Close: %v", err)
	}
}
