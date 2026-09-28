package portalis

import (
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"sync/atomic"
	"testing"
	"time"
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
			SessionID: "session",
			Data:      []byte("\x1b]7;/tmp/project\x07prompt"),
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
		SessionID: "session",
		Data:      []byte("\x1b]7;/tmp/project\x07"),
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

	em.Update(PtyOutputMsg{SessionID: "session", Generation: current - 1, Data: []byte("stale")})
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
