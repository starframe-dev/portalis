package portalis

import (
	"bufio"
	"bytes"
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

func debugLog(format string, args ...interface{}) {
	_ = format
	_ = args
}

// Pty wraps a pseudoterminal and forwards output to a channel.
type Pty struct {
	cmd            *exec.Cmd
	ptmx           *os.File
	reader         *bufio.Reader
	rawTrace       io.WriteCloser
	rawTraceChunks io.WriteCloser
	Output         chan []byte
	Errors         chan error
	done           chan struct{}
	readDone       chan struct{}
	closeOnce      sync.Once
	mu             sync.Mutex
	closeErr       error
	terminalErr    error

	lastRows int
	lastCols int
	setSize  func(*os.File, *pty.Winsize) error
}

// Spawn starts a new PTY with the given command, arguments and optional environment variables.
func Spawn(command string, args []string, env ...string) (*Pty, error) {
	return SpawnInDir(command, args, "", env...)
}

// SpawnInDir starts a new PTY in the given working directory.
func SpawnInDir(command string, args []string, dir string, env ...string) (*Pty, error) {
	cmd := exec.Command(command, args...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Env = append(cmd.Env, env...)
	if dir != "" {
		cmd.Dir = dir
	}
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("start pty: %w", err)
	}

	p := &Pty{
		cmd:    cmd,
		ptmx:   ptmx,
		reader: bufio.NewReader(ptmx),
		Output:   make(chan []byte, 64),
		Errors:   make(chan error, 1),
		done:     make(chan struct{}),
		readDone: make(chan struct{}),
	}
	if traceBase := os.Getenv("PORTALIS_RAW_TRACE"); traceBase != "" {
		tracePath := fmt.Sprintf("%s.%d", traceBase, cmd.Process.Pid)
		if trace, traceErr := os.OpenFile(tracePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); traceErr == nil {
			p.rawTrace = trace
			if chunks, chunksErr := os.OpenFile(tracePath+".chunks", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); chunksErr == nil {
				p.rawTraceChunks = chunks
			}
		}
	}

	go p.readLoop()
	return p, nil
}

// Write sends data to the PTY.
func (p *Pty) Write(data []byte) error {
	p.mu.Lock()
	ptmx := p.ptmx
	p.mu.Unlock()
	if ptmx == nil {
		return fmt.Errorf("pty closed")
	}
	_, err := ptmx.Write(data)
	return err
}

// Resize resizes the PTY and signals the child process about the change.
func (p *Pty) Resize(rows, cols int) error {
	if rows <= 0 || cols <= 0 || rows > 65535 || cols > 65535 {
		return fmt.Errorf("invalid pty size %dx%d", rows, cols)
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
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGWINCH)
	}
	return nil
}

// Close closes the PTY and kills the process.
func (p *Pty) Close() error {
	p.closeOnce.Do(func() {
		if p.done != nil {
			close(p.done)
		}

		p.mu.Lock()
		ptmx := p.ptmx
		p.ptmx = nil
		cmd := p.cmd
		p.mu.Unlock()

		if ptmx != nil {
			if err := ptmx.Close(); err != nil && p.closeErr == nil {
				p.closeErr = err
			}
		}
		if cmd != nil && cmd.Process != nil {
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) && p.closeErr == nil {
				p.closeErr = err
			}
			if _, err := cmd.Process.Wait(); err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) && !errors.Is(err, os.ErrProcessDone) && p.closeErr == nil {
					p.closeErr = err
				}
			}
		}
	})
	return p.closeErr
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

			if bytes.Contains(data, []byte("\x1b[6n")) {
				_ = p.Write([]byte("\x1b[1;1R"))
			}
			if bytes.Contains(data, []byte("\x1b[5n")) {
				_ = p.Write([]byte("\x1b[0n"))
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
			if err != io.EOF {
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
	SessionID  string
	Generation uint64
	Err        error
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
					p.mu.Lock()
					err := p.terminalErr
					p.mu.Unlock()
					return PtyExitMsg{SessionID: sessionID, Err: err}
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
