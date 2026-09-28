package portalis

import (
	"bufio"
	"bytes"
	"errors"
	"os"
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
	for _, size := range [][2]int{{0, 80}, {24, 0}, {-1, 80}, {24, 70000}} {
		if err := p.Resize(size[0], size[1]); err == nil {
			t.Fatalf("Resize(%d, %d) unexpectedly succeeded", size[0], size[1])
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
	lockDone := make(chan struct{})
	go func() {
		p.mu.Lock()
		p.mu.Unlock()
		close(lockDone)
	}()

	select {
	case <-lockDone:
	case <-time.After(time.Second):
		t.Fatal("Pty.Write held lifecycle mutex while blocked in I/O")
	}

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
