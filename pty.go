package portalis

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
)

func openPrivateRawTrace(path string) (*os.File, error) {
	trace, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := trace.Chmod(0o600); err != nil {
		_ = trace.Close()
		return nil, err
	}
	return trace, nil
}

// Pty wraps a pseudoterminal and forwards output to channels. Methods are
// safe for concurrent use; consume output through Listen or the exported
// channels, but not both, and call Listen at most once per Pty.
type Pty struct {
	cmd             *exec.Cmd
	ptmx            *os.File
	reader          *bufio.Reader
	rawTrace        io.WriteCloser
	rawTraceChunks  io.WriteCloser
	Output          chan []byte
	Errors          chan error
	done            chan struct{}
	readDone        chan struct{}
	writeQueue      chan ptyWrite
	writerDone      chan struct{}
	closeOnce       sync.Once
	waitOnce        sync.Once
	mu              sync.Mutex
	writeEnqueueMu  sync.Mutex
	writeBudget     ptyWriteBudget
	closeErr        error
	terminalErr     error
	waitErr         error
	writeGeneration uint64
	writeClosed     bool
	processExited   bool
	exitCode        int
	exitSignal      os.Signal

	lastRows int
	lastCols int
	setSize  func(*os.File, *pty.Winsize) error
}

type ptyWrite struct {
	generation uint64
	data       []byte
	result     chan error
}

const (
	ptyWriteQueueCapacity  = 64
	maxPtyQueuedWriteBytes = 100 << 20
)

var (
	errPtyClosed             = errors.New("pty closed")
	errStalePtyWrite         = errors.New("stale pty write generation")
	errPtyWriteExceedsBudget = errors.New("pty write exceeds queue byte limit")
)

type ptyWriteBudget struct {
	mu      sync.Mutex
	used    int
	changed chan struct{}
}

func newPtyWriteBudget() ptyWriteBudget {
	return ptyWriteBudget{changed: make(chan struct{})}
}

func (b *ptyWriteBudget) acquire(size int, done <-chan struct{}) bool {
	for {
		b.mu.Lock()
		if b.used+size <= maxPtyQueuedWriteBytes {
			b.used += size
			b.mu.Unlock()
			return true
		}
		changed := b.changed
		b.mu.Unlock()

		select {
		case <-done:
			return false
		case <-changed:
		}
	}
}

func (b *ptyWriteBudget) release(size int) {
	b.mu.Lock()
	b.used -= size
	close(b.changed)
	b.changed = make(chan struct{})
	b.mu.Unlock()
}

// Spawn starts a new PTY with the given command, arguments and optional environment variables.
func Spawn(command string, args []string, env ...string) (*Pty, error) {
	return SpawnInDir(command, args, "", env...)
}

// SpawnInDir starts a new PTY in the given working directory.
func SpawnInDir(command string, args []string, dir string, env ...string) (*Pty, error) {
	return spawnPtyWithSize(command, args, dir, defaultTerminalRows, defaultTerminalCols, env...)
}

func spawnPtyWithSize(command string, args []string, dir string, rows, cols int, env ...string) (*Pty, error) {
	if err := validateTerminalSize(rows, cols); err != nil {
		return nil, fmt.Errorf("invalid initial pty size %dx%d: %w", rows, cols, err)
	}
	cmd := exec.Command(command, args...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Env = append(cmd.Env, env...)
	if dir != "" {
		cmd.Dir = dir
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return nil, fmt.Errorf("start pty: %w", err)
	}

	p := &Pty{
		cmd:         cmd,
		ptmx:        ptmx,
		reader:      bufio.NewReader(ptmx),
		Output:      make(chan []byte, 64),
		Errors:      make(chan error, 1),
		done:        make(chan struct{}),
		readDone:    make(chan struct{}),
		writeQueue:  make(chan ptyWrite, ptyWriteQueueCapacity),
		writerDone:  make(chan struct{}),
		writeBudget: newPtyWriteBudget(),
		lastRows:    rows,
		lastCols:    cols,
	}
	if traceBase := os.Getenv("PORTALIS_RAW_TRACE"); traceBase != "" {
		tracePath := fmt.Sprintf("%s.%d", traceBase, cmd.Process.Pid)
		if trace, traceErr := openPrivateRawTrace(tracePath); traceErr == nil {
			p.rawTrace = trace
			if chunks, chunksErr := openPrivateRawTrace(tracePath + ".chunks"); chunksErr == nil {
				p.rawTraceChunks = chunks
			}
		}
	}

	go p.readLoop()
	go p.writeLoop()
	return p, nil
}

// Write sends data to the PTY through its ordered writer queue.
func (p *Pty) Write(data []byte) error {
	if p == nil {
		return errPtyClosed
	}
	p.mu.Lock()
	generation := p.writeGeneration
	p.mu.Unlock()
	return p.WriteForGeneration(generation, data)
}

// WriteForGeneration enqueues data only while generation is current. One
// writer goroutine performs all queued writes in admission order.
func (p *Pty) WriteForGeneration(generation uint64, data []byte) error {
	if p == nil {
		return errPtyClosed
	}
	if len(data) == 0 {
		return nil
	}
	if len(data) > maxPtyQueuedWriteBytes {
		return errPtyWriteExceedsBudget
	}
	if p.writeQueue == nil {
		return p.writeDirect(generation, data)
	}

	result, err := p.enqueueWrite(generation, data)
	if err != nil {
		return err
	}
	select {
	case err := <-result:
		return err
	case <-p.done:
		return errPtyClosed
	}
}

func (p *Pty) enqueueWrite(generation uint64, data []byte) (<-chan error, error) {
	p.writeEnqueueMu.Lock()
	defer p.writeEnqueueMu.Unlock()

	select {
	case <-p.done:
		return nil, errPtyClosed
	default:
	}

	p.mu.Lock()
	open := p.ptmx != nil && !p.writeClosed
	currentGeneration := p.writeGeneration
	p.mu.Unlock()
	if !open {
		return nil, errPtyClosed
	}
	if generation != currentGeneration {
		return nil, errStalePtyWrite
	}
	if !p.writeBudget.acquire(len(data), p.done) {
		return nil, errPtyClosed
	}

	request := ptyWrite{
		generation: generation,
		data:       append([]byte(nil), data...),
		result:     make(chan error, 1),
	}
	select {
	case p.writeQueue <- request:
		return request.result, nil
	case <-p.done:
		p.writeBudget.release(len(data))
		return nil, errPtyClosed
	}
}

func (p *Pty) writeDirect(generation uint64, data []byte) error {
	p.mu.Lock()
	ptmx := p.ptmx
	currentGeneration := p.writeGeneration
	closed := p.writeClosed
	p.mu.Unlock()
	if ptmx == nil || closed {
		return errPtyClosed
	}
	if generation != currentGeneration {
		return errStalePtyWrite
	}
	return writeAll(ptmx, data)
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func (p *Pty) setWriteGeneration(generation uint64) {
	p.mu.Lock()
	p.writeGeneration = generation
	p.mu.Unlock()
}

func (p *Pty) invalidateWrites() {
	p.mu.Lock()
	p.writeGeneration++
	p.writeClosed = true
	p.mu.Unlock()
}

func (p *Pty) writeLoop() {
	defer close(p.writerDone)
	defer p.drainQueuedWrites()

	for {
		select {
		case <-p.done:
			return
		case request := <-p.writeQueue:
			select {
			case <-p.done:
				p.finishQueuedWrite(request, errPtyClosed)
				continue
			default:
			}

			p.mu.Lock()
			ptmx := p.ptmx
			generation := p.writeGeneration
			closed := p.writeClosed
			p.mu.Unlock()

			var err error
			switch {
			case closed || ptmx == nil:
				err = errPtyClosed
			case request.generation != generation:
				err = errStalePtyWrite
			default:
				err = writeAll(ptmx, request.data)
			}
			p.finishQueuedWrite(request, err)
		}
	}
}

func (p *Pty) finishQueuedWrite(request ptyWrite, err error) {
	p.writeBudget.release(len(request.data))
	request.result <- err
}

func (p *Pty) drainQueuedWrites() {
	for {
		select {
		case request := <-p.writeQueue:
			p.writeBudget.release(len(request.data))
			select {
			case request.result <- errPtyClosed:
			default:
			}
		default:
			return
		}
	}
}

// Resize resizes the PTY. TIOCSWINSZ sends SIGWINCH to the foreground process group.
func (p *Pty) Resize(rows, cols int) error {
	if err := validateTerminalSize(rows, cols); err != nil {
		return fmt.Errorf("invalid pty size %dx%d: %w", rows, cols, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ptmx == nil {
		return fmt.Errorf("pty closed")
	}
	if rows == p.lastRows && cols == p.lastCols {
		return nil
	}

	setSize := p.setSize
	if setSize == nil {
		setSize = pty.Setsize
	}
	if err := setSize(p.ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}); err != nil {
		return err
	}
	p.lastRows = rows
	p.lastCols = cols
	return nil
}

// Close closes the PTY and kills the process.
func (p *Pty) Close() error {
	p.closeOnce.Do(func() {
		if p.done != nil {
			close(p.done)
		}
		p.writeEnqueueMu.Lock()
		p.mu.Lock()
		ptmx := p.ptmx
		p.ptmx = nil
		p.writeClosed = true
		p.writeGeneration++
		cmd := p.cmd
		p.mu.Unlock()
		p.writeEnqueueMu.Unlock()

		if ptmx != nil {
			if err := ptmx.Close(); err != nil && p.closeErr == nil {
				p.closeErr = err
			}
		}
		if cmd != nil && cmd.Process != nil {
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) && p.closeErr == nil {
				p.closeErr = err
			}
			if _, _, _, err := p.waitProcess(); err != nil && p.closeErr == nil {
				p.closeErr = err
			}
		}
		if p.writerDone != nil {
			<-p.writerDone
			p.drainQueuedWrites()
		}
	})
	return p.closeErr
}

func (p *Pty) waitProcess() (int, os.Signal, bool, error) {
	p.waitOnce.Do(func() {
		if p.cmd == nil || p.cmd.Process == nil {
			return
		}
		waitErr := p.cmd.Wait()
		state := p.cmd.ProcessState
		var exitCode int
		var exitSignal os.Signal
		processExited := state != nil
		if state != nil {
			exitCode = state.ExitCode()
			if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				exitSignal = status.Signal()
			}
		}
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			waitErr = nil
		}
		p.mu.Lock()
		p.exitCode = exitCode
		p.exitSignal = exitSignal
		p.processExited = processExited
		p.waitErr = waitErr
		p.mu.Unlock()
	})

	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitCode, p.exitSignal, p.processExited, p.waitErr
}

func (p *Pty) exitMessage(sessionID string) PtyExitMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	err := p.terminalErr
	if err == nil {
		err = p.waitErr
	}
	return PtyExitMsg{
		SessionID:     sessionID,
		ProcessExited: p.processExited,
		ExitCode:      p.exitCode,
		Signal:        p.exitSignal,
		Err:           err,
	}
}

func (p *Pty) readLoop() {
	buf := make([]byte, 4096)
	if p.readDone != nil {
		defer close(p.readDone)
	}
	defer close(p.Errors)
	if p.rawTrace != nil {
		defer p.rawTrace.Close()
	}
	if p.rawTraceChunks != nil {
		defer p.rawTraceChunks.Close()
	}
	for {
		select {
		case <-p.done:
			return
		default:
		}

		n, err := p.reader.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			if p.rawTrace != nil {
				_, _ = p.rawTrace.Write(data)
			}
			if p.rawTraceChunks != nil {
				_, _ = fmt.Fprintf(p.rawTraceChunks, "%d\n", len(data))
			}

			select {
			case p.Output <- data:
			case <-p.done:
				return
			}
		}
		if err != nil {
			select {
			case <-p.done:
				return
			default:
			}
			if errors.Is(err, io.EOF) || isExpectedPTYReadShutdown(err) {
				_, _, _, _ = p.waitProcess()
			} else {
				p.mu.Lock()
				p.terminalErr = err
				p.mu.Unlock()
				select {
				case p.Errors <- err:
				default:
				}
			}
			return
		}
	}
}

// PtyOutputMsg is sent when new data arrives from the PTY.
type PtyOutputMsg struct {
	SessionID  string
	Generation uint64
	Data       []byte
}

// PtyExitMsg is sent when the PTY process exits.
type PtyExitMsg struct {
	SessionID     string
	Generation    uint64
	ProcessExited bool
	ExitCode      int
	Signal        os.Signal
	Err           error
}

// PtyErrorMsg reports an error from a standalone PTY helper such as SendBytes.
type PtyErrorMsg struct {
	Err error
}

// SendBytes sends raw bytes to the PTY.
func SendBytes(p *Pty, data []byte) tea.Cmd {
	return func() tea.Msg {
		if p == nil {
			return nil
		}
		if err := p.Write(data); err != nil {
			return PtyErrorMsg{Err: err}
		}
		return nil
	}
}

// Listen returns a bubbletea command that waits for one ordered PTY read.
// readLoop already limits reads to 4 KiB; keeping that boundary prevents a
// 64 KiB Parser.Feed call from blocking the UI update loop.
// Listen returns a command that emits one PTY output or exit message. Call it
// serially and keep its returned command chain alive until the PTY exits.
func (p *Pty) Listen(sessionID string) tea.Cmd {
	return func() tea.Msg {
		// Spawned PTYs use readDone so process completion cannot overtake
		// already-buffered output. Drain Output first, then report the terminal
		// error/EOF only after the read loop has finished.
		if p.readDone != nil {
			for {
				select {
				case data := <-p.Output:
					return PtyOutputMsg{SessionID: sessionID, Data: data}
				case <-p.readDone:
					select {
					case data := <-p.Output:
						return PtyOutputMsg{SessionID: sessionID, Data: data}
					default:
					}
					return p.exitMessage(sessionID)
				}
			}
		}

		// Compatibility path for tests or manually-constructed Pty values.
		select {
		case data, ok := <-p.Output:
			if !ok {
				return PtyExitMsg{SessionID: sessionID}
			}
			return PtyOutputMsg{SessionID: sessionID, Data: data}
		case err, ok := <-p.Errors:
			if !ok {
				return PtyExitMsg{SessionID: sessionID}
			}
			return PtyExitMsg{SessionID: sessionID, Err: err}
		}
	}
}
