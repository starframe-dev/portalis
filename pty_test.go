package portalis

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	creackpty "github.com/creack/pty"
)

type traceRecorder struct {
	bytes.Buffer
	closed bool
}

func (r *traceRecorder) Close() error {
	r.closed = true
	return nil
}

func TestOpenPrivateRawTraceRestrictsExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.bin")
	if err := os.WriteFile(path, []byte("old trace"), 0o644); err != nil {
		t.Fatal(err)
	}
	trace, err := openPrivateRawTrace(path)
	if err != nil {
		t.Fatal(err)
	}
	defer trace.Close()
	info, err := trace.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("trace mode = %04o, want 0600", got)
	}
}

func TestPtyReadLoopCopiesRawBytesToTrace(t *testing.T) {
	const raw = "\x1b[38;2;139;26;26mstatus\x1b[0m"
	recorder := &traceRecorder{}
	chunks := &traceRecorder{}
	p := &Pty{
		reader:         bufio.NewReader(bytes.NewBufferString(raw)),
		rawTrace:       recorder,
		rawTraceChunks: chunks,
		Output:         make(chan []byte, 1),
		Errors:         make(chan error, 1),
		done:           make(chan struct{}),
	}

	p.readLoop()

	if got := recorder.String(); got != raw {
		t.Fatalf("raw trace = %q, want %q", got, raw)
	}
	if !recorder.closed {
		t.Fatal("raw trace was not closed after read loop exit")
	}
	if got, want := chunks.String(), "27\n"; got != want {
		t.Fatalf("chunk trace = %q, want %q", got, want)
	}
	if !chunks.closed {
		t.Fatal("chunk trace was not closed after read loop exit")
	}
	if got := string(<-p.Output); got != raw {
		t.Fatalf("PTY output = %q, want %q", got, raw)
	}
}

func TestPtyListenPreservesReadBoundaries(t *testing.T) {
	p := &Pty{
		Output: make(chan []byte, 2),
		Errors: make(chan error, 1),
	}
	p.Output <- []byte("tmux")
	p.Output <- []byte("frame")

	for _, expected := range []string{"tmux", "frame"} {
		msg := p.Listen("session")()
		output, ok := msg.(PtyOutputMsg)
		if !ok {
			t.Fatalf("message type = %T, want PtyOutputMsg", msg)
		}
		if output.SessionID != "session" {
			t.Fatalf("session = %q, want session", output.SessionID)
		}
		if got := string(output.Data); got != expected {
			t.Fatalf("PTY output = %q, want %q", got, expected)
		}
	}
}

func TestResizeDoesNotDoubleSignal(t *testing.T) {
	script := "count=0; trap 'count=$((count+1))' WINCH; printf READY; while [ \"$count\" -eq 0 ]; do sleep 0.05; done; sleep 0.2; printf 'COUNT:%s\\n' \"$count\"; sleep 5"
	p, err := Spawn("/bin/bash", []string{"-c", script})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	readOutput := func(timeout time.Duration) ([]byte, error) {
		message := make(chan any, 1)
		go func() { message <- p.Listen("resize-signal")() }()
		select {
		case msg := <-message:
			switch msg := msg.(type) {
			case PtyOutputMsg:
				return msg.Data, nil
			case PtyExitMsg:
				return nil, fmt.Errorf("PTY exited during resize signal test: %v", msg.Err)
			default:
				return nil, fmt.Errorf("unexpected PTY message %T", msg)
			}
		case <-time.After(timeout):
			return nil, errors.New("timed out waiting for resize signal output")
		}
	}

	var output []byte
	deadline := time.Now().Add(time.Second)
	for !bytes.Contains(output, []byte("READY")) {
		chunk, err := readOutput(time.Until(deadline))
		if err != nil {
			t.Fatal(err)
		}
		output = append(output, chunk...)
	}
	if err := p.Resize(40, 120); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for !bytes.Contains(output, []byte("COUNT:")) {
		chunk, err := readOutput(time.Until(deadline))
		if err != nil {
			t.Fatal(err)
		}
		output = append(output, chunk...)
	}
	if !bytes.Contains(output, []byte("COUNT:1")) {
		t.Fatalf("resize delivered more than one SIGWINCH: output %q", output)
	}
}

func TestPtyResizeAppliesEveryDistinctFinalSize(t *testing.T) {
	var applied []creackpty.Winsize
	p := &Pty{
		ptmx: &os.File{},
		setSize: func(_ *os.File, size *creackpty.Winsize) error {
			applied = append(applied, *size)
			return nil
		},
	}

	if err := p.Resize(24, 80); err != nil {
		t.Fatal(err)
	}
	if err := p.Resize(40, 120); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 2 {
		t.Fatalf("applied resize count = %d, want 2", len(applied))
	}
	if got := applied[1]; got.Rows != 40 || got.Cols != 120 {
		t.Fatalf("final physical size = %dx%d, want 40x120", got.Rows, got.Cols)
	}
	if p.lastRows != 40 || p.lastCols != 120 {
		t.Fatalf("recorded size = %dx%d, want 40x120", p.lastRows, p.lastCols)
	}
}

func TestPtyCloseIsIdempotent(t *testing.T) {
	p := &Pty{done: make(chan struct{})}
	if err := p.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestPtyResizeRejectsInvalidDimensions(t *testing.T) {
	p := &Pty{ptmx: &os.File{}}
	for _, size := range [][2]int{
		{0, 80}, {24, 0}, {-1, 80}, {24, 70000},
		{65536, 1}, {1, 65536}, {1000000, 1000000}, {513, 512},
	} {
		if err := p.Resize(size[0], size[1]); err == nil {
			t.Errorf("Resize(%d, %d) unexpectedly succeeded", size[0], size[1])
		}
	}
}

func TestPtyResizeAcceptsMaximumSingleDimension(t *testing.T) {
	for _, size := range [][2]int{{65535, 1}, {1, 65535}} {
		p := &Pty{
			ptmx: &os.File{},
			setSize: func(_ *os.File, _ *creackpty.Winsize) error {
				return nil
			},
		}
		if err := p.Resize(size[0], size[1]); err != nil {
			t.Fatalf("Resize(%d, %d): %v", size[0], size[1], err)
		}
	}
}

func TestPtyListenHandlesClosedErrorChannel(t *testing.T) {
	p := &Pty{
		Output: make(chan []byte),
		Errors: make(chan error),
	}
	close(p.Errors)
	msg := p.Listen("session")()
	exit, ok := msg.(PtyExitMsg)
	if !ok {
		t.Fatalf("message type = %T, want PtyExitMsg", msg)
	}
	if exit.SessionID != "session" {
		t.Fatalf("session = %q, want session", exit.SessionID)
	}
}

func TestPtyListenDrainsBufferedOutputBeforeExit(t *testing.T) {
	p := &Pty{
		Output:   make(chan []byte, 2),
		Errors:   make(chan error, 1),
		readDone: make(chan struct{}),
	}
	p.Output <- []byte("final")
	p.terminalErr = errors.New("terminal closed")
	close(p.readDone)

	msg := p.Listen("session")()
	out, ok := msg.(PtyOutputMsg)
	if !ok || string(out.Data) != "final" {
		t.Fatalf("first message = %#v, want final PtyOutputMsg", msg)
	}

	msg = p.Listen("session")()
	exit, ok := msg.(PtyExitMsg)
	if !ok {
		t.Fatalf("second message type = %T, want PtyExitMsg", msg)
	}
	if exit.Err == nil || exit.Err.Error() != "terminal closed" {
		t.Fatalf("exit error = %v, want terminal closed", exit.Err)
	}
}

func TestPTYWritesAreSerialized(t *testing.T) {
	p, err := Spawn("/bin/sh", []string{"-c", "stty -echo -icanon min 1 time 0; printf READY; cat"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	readOutput := func(timeout time.Duration) ([]byte, error) {
		message := make(chan any, 1)
		go func() { message <- p.Listen("ordered-write")() }()
		select {
		case msg := <-message:
			switch msg := msg.(type) {
			case PtyOutputMsg:
				return msg.Data, nil
			case PtyExitMsg:
				return nil, fmt.Errorf("PTY exited while reading ordered writes: %w", msg.Err)
			default:
				return nil, fmt.Errorf("unexpected PTY message %T", msg)
			}
		case <-time.After(timeout):
			return nil, errors.New("timed out waiting for PTY output")
		}
	}

	ready, err := readOutput(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(ready) != "READY" {
		t.Fatalf("PTY readiness output = %q, want READY", ready)
	}

	writes := []struct {
		name string
		data []byte
	}{
		{name: "keyboard", data: []byte("k")},
		{name: "DSR reply", data: []byte("\x1b[1;1R")},
		{name: "focus", data: []byte("\x1b[I")},
		{name: "paste", data: []byte("\x1b[200~paste\x1b[201~")},
	}
	var want []byte
	results := make([]<-chan error, 0, len(writes))
	for _, write := range writes {
		result, err := p.enqueueWrite(0, write.data)
		if err != nil {
			t.Fatalf("enqueue %s: %v", write.name, err)
		}
		results = append(results, result)
		want = append(want, write.data...)
	}
	for i, result := range results {
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("write %s: %v", writes[i].name, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("write %s did not complete", writes[i].name)
		}
	}

	var got []byte
	deadline := time.Now().Add(time.Second)
	for len(got) < len(want) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("child received %q, want exact ordered bytes %q", got, want)
		}
		chunk, err := readOutput(remaining)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, chunk...)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("child received %q, want exact ordered bytes %q", got, want)
	}
}

func TestPTYWriterDropsStaleGeneration(t *testing.T) {
	p, err := Spawn("/bin/sh", []string{"-c", "stty -echo; cat"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.setWriteGeneration(8)

	if err := p.WriteForGeneration(7, []byte("stale")); !errors.Is(err, errStalePtyWrite) {
		t.Fatalf("stale-generation write error = %v, want %v", err, errStalePtyWrite)
	}
}

func TestPTYWriterCloseUnblocksPendingWrite(t *testing.T) {
	p, err := Spawn("sleep", []string{"5"})
	if err != nil {
		t.Fatal(err)
	}

	writeDone := make(chan error, 1)
	go func() { writeDone <- p.Write(bytes.Repeat([]byte("x"), 8<<20)) }()
	time.Sleep(50 * time.Millisecond)

	closeDone := make(chan error, 1)
	go func() { closeDone <- p.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close deadlocked while writer was blocked")
	}
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("pending write did not unblock after Close")
	}
}

func TestPtyWriteDoesNotHoldLifecycleMutexWhileBlocked(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	p := &Pty{ptmx: w, done: make(chan struct{})}

	writeDone := make(chan struct{})
	go func() {
		_ = p.Write(bytes.Repeat([]byte("x"), 8<<20))
		close(writeDone)
	}()

	// Give the large pipe write time to block with no reader.
	time.Sleep(50 * time.Millisecond)
	if !p.mu.TryLock() {
		t.Fatal("Pty.Write held lifecycle mutex while blocked in I/O")
	}
	p.mu.Unlock()

	_ = w.Close()
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("blocked write did not unblock after file close")
	}
}

func TestSendBytesReturnsWriteError(t *testing.T) {
	p := &Pty{}
	msg := SendBytes(p, []byte("x"))()
	got, ok := msg.(PtyErrorMsg)
	if !ok || got.Err == nil {
		t.Fatalf("SendBytes message = %#v, want PtyErrorMsg", msg)
	}
}

func TestSpawnWithSizeSetsWinsizeBeforeChildStarts(t *testing.T) {
	const rows, cols = 37, 91
	p, err := spawnPtyWithSize("/bin/sh", []string{"-c", "stty size"}, "", rows, cols)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	var output strings.Builder
	for {
		msg := p.Listen("session")()
		switch msg := msg.(type) {
		case PtyOutputMsg:
			output.Write(msg.Data)
		case PtyExitMsg:
			if !strings.Contains(output.String(), "37 91") {
				t.Fatalf("child observed initial PTY size %q, want 37 91", output.String())
			}
			return
		default:
			t.Fatalf("unexpected PTY message %T", msg)
		}
	}
}

func TestSpawnTERMDefaultsAndAllowsOverride(t *testing.T) {
	for _, test := range []struct {
		name string
		env  []string
		want string
	}{
		{name: "default", want: "xterm-256color"},
		{name: "explicit-override", env: []string{"TERM=screen-256color"}, want: "screen-256color"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, err := Spawn("/bin/sh", []string{"-c", "printf '%s' \"$TERM\""}, test.env...)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()

			var output strings.Builder
			for {
				msg := p.Listen("term")()
				switch msg := msg.(type) {
				case PtyOutputMsg:
					output.Write(msg.Data)
				case PtyExitMsg:
					if msg.Err != nil || !msg.ProcessExited {
						t.Fatalf("TERM probe exit = %#v", msg)
					}
					if got := output.String(); got != test.want {
						t.Fatalf("TERM = %q, want %q", got, test.want)
					}
					return
				default:
					t.Fatalf("unexpected PTY message %T", msg)
				}
			}
		})
	}
}

func TestNormalExitAndNonZeroExitAreDistinct(t *testing.T) {
	for _, test := range []struct {
		name     string
		command  string
		exitCode int
		signal   os.Signal
	}{
		{name: "normal", command: "exit 0", exitCode: 0},
		{name: "nonzero", command: "exit 42", exitCode: 42},
		{name: "signal", command: "kill -TERM $$", exitCode: -1, signal: syscall.SIGTERM},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, err := Spawn("/bin/sh", []string{"-c", test.command})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()

			for {
				msg := p.Listen("exit-status")()
				if exit, ok := msg.(PtyExitMsg); ok {
					if exit.Err != nil || !exit.ProcessExited || exit.ExitCode != test.exitCode || exit.Signal != test.signal {
						t.Fatalf("exit = %#v, want process exit code %d and signal %v", exit, test.exitCode, test.signal)
					}
					return
				}
				if _, ok := msg.(PtyOutputMsg); !ok {
					t.Fatalf("unexpected PTY message %T", msg)
				}
			}
		})
	}
}

func TestSpawnEndToEndPreservesFinalOutput(t *testing.T) {
	p, err := Spawn("/bin/sh", []string{"-c", "printf 'hello\\033[31mred\\033[0m'"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	screen := NewScreen(2, 30)
	parser := NewParser(screen)
	for i := 0; i < 32; i++ {
		msg := p.Listen("session")()
		switch msg := msg.(type) {
		case PtyOutputMsg:
			parser.Feed(msg.Data)
		case PtyExitMsg:
			if got := screen.LineText(0); !strings.Contains(got, "hellored") {
				t.Fatalf("final PTY output = %q, want hellored", got)
			}
			if !msg.ProcessExited || msg.ExitCode != 0 || msg.Err != nil {
				t.Fatalf("process exit = %#v, want normal exit code 0", msg)
			}
			return
		default:
			t.Fatalf("unexpected PTY message %T", msg)
		}
	}
	t.Fatal("PTY did not terminate within message bound")
}
