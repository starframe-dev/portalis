package portalis

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
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

type failingTraceWriter struct {
	closed bool
}

type partialWriteRecorder struct {
	bytes.Buffer
	max int
}

func (w *partialWriteRecorder) Write(data []byte) (int, error) {
	if len(data) > w.max {
		data = data[:w.max]
	}
	return w.Buffer.Write(data)
}

type noProgressWriter struct{}

func (noProgressWriter) Write([]byte) (int, error) { return 0, nil }

func (w *failingTraceWriter) Write([]byte) (int, error) {
	return 0, errors.New("trace storage unavailable")
}

func (w *failingTraceWriter) Close() error {
	w.closed = true
	return nil
}

func TestOpenPrivateRawTraceRestrictsNewFilesAndRejectsCollision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.bin")
	trace, err := openPrivateRawTrace(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := trace.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("trace mode = %04o, want 0600", got)
	}
	if err := trace.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := openPrivateRawTrace(path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing trace path error = %v, want os.ErrExist", err)
	}
}

func TestSpawnUsesBoundedRawTraceConfiguration(t *testing.T) {
	base := filepath.Join(t.TempDir(), "pty-trace")
	t.Setenv("PORTALIS_RAW_TRACE", base)
	t.Setenv("PORTALIS_RAW_TRACE_MAX_BYTES", "4")
	t.Setenv("PORTALIS_RAW_TRACE_MAX_FILES", "2")
	p, err := Spawn("/bin/sh", []string{"-c", "printf 12345678"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for {
		msg := p.Listen("trace-test")()
		switch value := msg.(type) {
		case PtyOutputMsg:
		case PtyWarningMsg:
			t.Fatalf("unexpected trace warning: %v", value.Err)
		case PtyExitMsg:
			goto exited
		default:
			t.Fatalf("unexpected PTY message %T", msg)
		}
	}

exited:
	tracePath := fmt.Sprintf("%s.%d", base, p.State().PID)
	first, err := os.ReadFile(tracePath + ".1")
	if err != nil {
		t.Fatal(err)
	}
	last, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(append(first, last...)); got != "12345678" {
		t.Fatalf("rotated PTY trace = %q, want 12345678", got)
	}
	for _, path := range []string{tracePath, tracePath + ".1", tracePath + ".chunks"} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", filepath.Base(path), info.Mode().Perm())
		}
		if info.Size() > 4 {
			t.Errorf("%s size = %d, exceeds configured limit", filepath.Base(path), info.Size())
		}
	}
}

func TestRotatingTraceCapsBytesFilesAndPermissions(t *testing.T) {
	base := filepath.Join(t.TempDir(), "raw")
	trace, err := openRotatingTrace(base, rawTraceConfig{maxBytes: 5, maxFiles: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trace.Write([]byte("abcde")); err != nil {
		t.Fatal(err)
	}
	if _, err := trace.Write([]byte("fg")); err != nil {
		t.Fatal(err)
	}
	if _, err := trace.Write([]byte("hijklmnop")); err != nil {
		t.Fatal(err)
	}
	if err := trace.Close(); err != nil {
		t.Fatal(err)
	}

	wantContents := map[string]string{
		base:        "p",
		base + ".1": "klmno",
		base + ".2": "fghij",
	}
	var totalBytes int64
	for path, want := range wantContents {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Errorf("%s = %q, want %q", filepath.Base(path), data, want)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s permissions = %04o, want 0600", filepath.Base(path), mode)
		}
		if info.Size() > 5 {
			t.Errorf("%s size = %d, exceeds 5-byte limit", filepath.Base(path), info.Size())
		}
		totalBytes += info.Size()
	}
	if totalBytes > 15 {
		t.Fatalf("retained trace bytes = %d, exceeds configured cap", totalBytes)
	}
}

func TestRawTraceConfigRejectsUnboundedEnvironmentValues(t *testing.T) {
	t.Setenv("PORTALIS_RAW_TRACE_MAX_BYTES", "0")
	t.Setenv("PORTALIS_RAW_TRACE_MAX_FILES", "999999")
	config, warnings := rawTraceConfigFromEnv()
	if config.maxBytes != defaultRawTraceMaxBytes || config.maxFiles != defaultRawTraceMaxFiles || len(warnings) != 2 {
		t.Fatalf("trace config = %+v, warnings = %v", config, warnings)
	}
}

func TestWriteAllHandlesPartialAndZeroWrites(t *testing.T) {
	partial := &partialWriteRecorder{max: 2}
	if err := writeAll(partial, []byte("partial writes")); err != nil {
		t.Fatal(err)
	}
	if got := partial.String(); got != "partial writes" {
		t.Fatalf("partial writer received %q", got)
	}
	if err := writeAll(noProgressWriter{}, []byte("x")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("zero-progress writer error = %v, want io.ErrShortWrite", err)
	}
}

func TestTraceWriteFailureWarnsWithoutDroppingPTYOutput(t *testing.T) {
	trace := &failingTraceWriter{}
	p := &Pty{
		reader:   bufio.NewReader(bytes.NewBufferString("output")),
		rawTrace: trace,
		output:   make(chan []byte, 1),
		errors:   make(chan error, 1),
		warnings: make(chan error, 4),
		readDone: make(chan struct{}),
		done:     make(chan struct{}),
	}
	p.readLoop()
	if !trace.closed {
		t.Fatal("failed trace writer was not closed")
	}

	sawOutput := false
	sawWarning := false
	for range 2 {
		msg := p.Listen("session")()
		switch value := msg.(type) {
		case PtyOutputMsg:
			if string(value.Data) != "output" {
				t.Fatalf("PTY output = %q, want output", value.Data)
			}
			sawOutput = true
		case PtyWarningMsg:
			if value.Err == nil || !strings.Contains(value.Err.Error(), "trace storage unavailable") {
				t.Fatalf("trace warning = %v", value.Err)
			}
			sawWarning = true
		default:
			t.Fatalf("unexpected PTY message %T", msg)
		}
	}
	if !sawOutput || !sawWarning {
		t.Fatalf("observed output=%v warning=%v, want both", sawOutput, sawWarning)
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
		output:         make(chan []byte, 1),
		errors:         make(chan error, 1),
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
	if got := string(<-p.output); got != raw {
		t.Fatalf("PTY output = %q, want %q", got, raw)
	}
}

func TestPtyListenPreservesReadBoundaries(t *testing.T) {
	p := &Pty{
		output: make(chan []byte, 2),
		errors: make(chan error, 1),
	}
	p.output <- []byte("tmux")
	p.output <- []byte("frame")

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
		output: make(chan []byte),
		errors: make(chan error),
	}
	close(p.errors)
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
		output:   make(chan []byte, 2),
		errors:   make(chan error, 1),
		readDone: make(chan struct{}),
	}
	p.output <- []byte("final")
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

func TestPTYConcurrentWritesRemainAtomic(t *testing.T) {
	const writerCount = 100
	p, err := Spawn("/bin/sh", []string{"-c", "stty -echo -icanon min 1 time 0; printf READY; cat"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	readOutput := func(timeout time.Duration) ([]byte, error) {
		message := make(chan any, 1)
		go func() { message <- p.Listen("concurrent-write")() }()
		select {
		case msg := <-message:
			switch value := msg.(type) {
			case PtyOutputMsg:
				return value.Data, nil
			case PtyExitMsg:
				return nil, fmt.Errorf("PTY exited: %w", value.Err)
			default:
				return nil, fmt.Errorf("unexpected PTY message %T", msg)
			}
		case <-time.After(timeout):
			return nil, errors.New("timed out waiting for PTY output")
		}
	}
	ready, err := readOutput(time.Second)
	if err != nil || string(ready) != "READY" {
		t.Fatalf("PTY readiness = %q, %v", ready, err)
	}

	start := make(chan struct{})
	writeErrs := make(chan error, writerCount)
	payloads := make([][]byte, writerCount)
	var expectedBytes int
	for i := range writerCount {
		payload := []byte(fmt.Sprintf("<%03d:%016x>", i, i))
		payloads[i] = payload
		expectedBytes += len(payload)
		go func(data []byte) {
			<-start
			writeErrs <- p.Write(data)
		}(payload)
	}
	close(start)
	for range writerCount {
		if err := <-writeErrs; err != nil {
			t.Fatalf("concurrent PTY write: %v", err)
		}
	}

	var got []byte
	deadline := time.Now().Add(2 * time.Second)
	for len(got) < expectedBytes {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("received %d/%d echoed bytes", len(got), expectedBytes)
		}
		chunk, err := readOutput(remaining)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, chunk...)
	}
	if len(got) != expectedBytes {
		t.Fatalf("echoed bytes = %d, want %d", len(got), expectedBytes)
	}

	remaining := make(map[string]int, writerCount)
	for _, payload := range payloads {
		remaining[string(payload)]++
	}
	for offset := 0; offset < len(got); {
		length := len(payloads[0])
		if offset+length > len(got) {
			t.Fatalf("truncated write payload at offset %d", offset)
		}
		key := string(got[offset : offset+length])
		if remaining[key] == 0 {
			t.Fatalf("interleaved or unknown payload at offset %d: %q", offset, key)
		}
		remaining[key]--
		offset += length
	}
	for payload, count := range remaining {
		if count != 0 {
			t.Fatalf("payload %q arrived %d extra times", payload, count)
		}
	}
}

func TestPTYWriterDropsStaleGeneration(t *testing.T) {
	p, err := Spawn("/bin/sh", []string{"-c", "stty -echo; cat"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.setWriteGeneration(8)

	if err := p.writeForGeneration(7, []byte("stale")); !errors.Is(err, errStalePtyWrite) {
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
	select {
	case <-p.readDone:
	default:
		t.Fatal("Close returned before the PTY reader goroutine exited")
	}
}

func TestPTYWriterCloseUnblocksSaturatedQueue(t *testing.T) {
	p, err := Spawn("sleep", []string{"5"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	active, err := p.enqueueWrite(0, bytes.Repeat([]byte{'x'}, 8<<20))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for len(p.writeQueue) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("writer did not take the blocking payload")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-active:
		t.Fatalf("blocking payload completed unexpectedly: %v", err)
	default:
	}

	queued := make([]<-chan error, 0, ptyWriteQueueCapacity)
	for i := 0; i < ptyWriteQueueCapacity; i++ {
		result, err := p.enqueueWrite(0, []byte("q"))
		if err != nil {
			t.Fatalf("enqueue saturated request %d: %v", i, err)
		}
		queued = append(queued, result)
	}
	if len(p.writeQueue) != ptyWriteQueueCapacity {
		t.Fatalf("queued requests = %d, want %d", len(p.writeQueue), ptyWriteQueueCapacity)
	}

	extraDone := make(chan error, 1)
	go func() {
		_, err := p.enqueueWrite(0, []byte("blocked"))
		extraDone <- err
	}()
	time.Sleep(10 * time.Millisecond)
	closeDone := make(chan error, 1)
	go func() { closeDone <- p.Close() }()
	select {
	case err := <-extraDone:
		if !errors.Is(err, errPtyClosed) {
			t.Fatalf("blocked enqueue error = %v, want closed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock the saturated-queue producer")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close deadlocked on a saturated write queue")
	}
	for _, result := range append([]<-chan error{active}, queued...) {
		select {
		case <-result:
		case <-time.After(time.Second):
			t.Fatal("queued write result was not completed during Close")
		}
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
		{name: "default", want: "ansi"},
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
