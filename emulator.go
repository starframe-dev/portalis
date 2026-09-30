package portalis

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// asciiArtIcon is shown when the chat session has been stopped.
const asciiArtIcon = `
     _____
    /     \
   /       \
  /_________\
   |  | |  |
   |  | |  |
   \______/
`

const maxCommandHistory = 1000

// ResizeMsg is sent by a host when the emulator's allocated rectangle changes.
// It carries the content size in cells (without borders or padding).
type ResizeMsg struct {
	Width  int
	Height int
}

// Emulator is a built-in terminal emulator. It runs a shell/command in a PTY
// and renders the output. It is independent of any UI framework; hosts feed it
// keyboard/mouse/resize messages and call View(width, height) to render it.
type Emulator struct {
	sessionID string
	chatName  string
	cmd       string
	args      []string

	screen  *Screen
	parser  *Parser
	pty     *Pty
	focused bool

	width            int
	height           int
	requestedWidth   int
	requestedHeight  int
	resizeGeneration uint64
	resizeMu         sync.Mutex

	// stopped indicates the underlying session has been terminated and the
	// panel should show the idle ASCII art icon instead of terminal output.
	stopped bool

	// cwd/title are the last validated OSC metadata from the child.
	cwd                 WorkingDirectory
	cwdSet              bool
	title               string
	pendingCWDChanges   []WorkingDirectory
	pendingTitleChanges []string
	pendingResponses    [][]byte

	// commandHistory holds commands entered in this terminal.
	commandHistory []string

	// Callbacks run outside mu and may re-enter the Emulator.
	onCWDChange             func(WorkingDirectory)
	onTitleChange           func(string)
	onCommandHistoryChanged func([]string)
	onError                 func(error)
	onExit                  func(PtyExitMsg)

	// initialCWD is set before Start and used to chdir the PTY process.
	initialCWD string

	// startEnv holds extra environment variables applied on Start when the
	// caller does not pass explicit env vars. Set via SetStartEnv before the
	// process is spawned; this keeps the environment consistent no matter
	// which Start* variant ends up launching the PTY.
	startEnv []string

	// scrollbackLimit caps the screen scrollback buffer.
	scrollbackLimit    int
	scrollbackLimitSet bool

	// listenerPending prevents multiple Bubble Tea commands from reading the
	// same PTY output channel concurrently.
	listenerPending     bool
	listenerGeneration  uint64
	lifecycleGeneration uint64

	// Drag-select state: remember the press position and whether an
	// actual drag is in progress. Selection starts only when the mouse
	// moves more than one cell from the press position.
	pressX, pressY     int
	dragSelecting      bool
	clipboardPolicy    ClipboardTempPolicy
	clipboardTempStore *clipboardTempStore
	readClipboard      func(string) (string, string, error)

	mu sync.RWMutex
}

// RenderTickMsg is kept for compatibility with hosts that may still route
// queued tick messages from an older emulator instance.
type RenderTickMsg struct{ SessionID string }

// NewEmulator creates a new terminal emulator for the given session.
// If command is empty, it tries to find a shell (bash, sh).

func NewEmulator(sessionID, chatName, command string, args []string) *Emulator {
	if command == "" {
		command, args = defaultShell()
	}
	return &Emulator{
		sessionID:       sessionID,
		chatName:        chatName,
		cmd:             command,
		args:            append([]string(nil), args...),
		width:           defaultTerminalCols,
		height:          defaultTerminalRows,
		requestedWidth:  defaultTerminalCols,
		requestedHeight: defaultTerminalRows,
		clipboardPolicy: DefaultClipboardTempPolicy(),
	}
}

// Start begins spawning the PTY process. Returns PtyReadyMsg when done.
func (e *Emulator) Start() tea.Cmd {
	return e.StartWithEnv(nil)
}

// SetStartEnv records extra environment variables to use when the PTY is
// spawned without explicit env vars (Start, StartWithEnv(nil), StartSync(nil)).
// Must be called before the process starts; later calls have no effect on an
// already running PTY.
func (e *Emulator) SetStartEnv(env []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.startEnv = append([]string(nil), env...)
}

// StartEnv returns the env vars recorded via SetStartEnv (nil when unset).
func (e *Emulator) StartEnv() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]string(nil), e.startEnv...)
}

// SessionID returns the immutable session identifier supplied at construction.
func (e *Emulator) SessionID() string { return e.sessionID }

// ChatName returns the immutable display name supplied at construction.
func (e *Emulator) ChatName() string { return e.chatName }

// SetOnCWDChange registers a callback for validated OSC 7 directory changes.
func (e *Emulator) SetOnCWDChange(fn func(WorkingDirectory)) {
	e.mu.Lock()
	e.onCWDChange = fn
	e.mu.Unlock()
}

// SetOnTitleChange registers a callback for validated OSC 0/2 terminal titles.
func (e *Emulator) SetOnTitleChange(fn func(string)) {
	e.mu.Lock()
	e.onTitleChange = fn
	e.mu.Unlock()
}

// SetOnCommandHistoryChanged registers a callback for heuristic command captures.
func (e *Emulator) SetOnCommandHistoryChanged(fn func([]string)) {
	e.mu.Lock()
	e.onCommandHistoryChanged = fn
	e.mu.Unlock()
}

// SetOnError registers a callback for PTY, clipboard, and terminal errors.
func (e *Emulator) SetOnError(fn func(error)) {
	e.mu.Lock()
	e.onError = fn
	e.mu.Unlock()
}

// SetOnExit registers a callback invoked when the child process has exited.
func (e *Emulator) SetOnExit(fn func(PtyExitMsg)) {
	e.mu.Lock()
	e.onExit = fn
	e.mu.Unlock()
}

// effectiveEnv resolves the env vars to spawn with: explicit extraEnv wins,
// otherwise the recorded startEnv is used.
func (e *Emulator) effectiveEnv(extraEnv []string) []string {
	if extraEnv != nil {
		return append([]string(nil), extraEnv...)
	}
	return append([]string(nil), e.startEnv...)
}

// SetScrollbackLimit sets the maximum number of scrollback lines. Non-positive
// values disable the line-count limit; the Screen cell-count cap remains active.
// Call before Start or update the existing screen immediately after Start.
func (e *Emulator) SetScrollbackLimit(limit int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.scrollbackLimit = limit
	e.scrollbackLimitSet = true
	if e.screen != nil {
		e.screen.SetScrollbackLimit(limit)
	}
}

// StartSync spawns the PTY process synchronously with extra environment variables.
// Unlike StartWithEnv, it does not return a tea.Cmd — the PTY is ready immediately.
// It returns an error if spawning or initial resizing fails; a resize failure
// leaves the spawned PTY attached with its last successfully applied size.
func (e *Emulator) StartSync(extraEnv []string) error {
	e.mu.Lock()
	if e.pty != nil {
		e.mu.Unlock()
		return nil
	}
	e.lifecycleGeneration++
	lifecycle := e.lifecycleGeneration
	e.resetTerminalLocked()
	generation := e.listenerGeneration
	env := e.effectiveEnv(extraEnv)
	command := e.cmd
	args := append([]string(nil), e.args...)
	initialCWD := e.initialCWD
	sessionID := e.sessionID
	rows, cols := e.requestedHeight, e.requestedWidth
	e.mu.Unlock()

	pty, err := spawnPtyConfig(command, args, initialCWD, sessionID, rows, cols, env)
	if err != nil {
		e.mu.Lock()
		if lifecycle == e.lifecycleGeneration && generation == e.listenerGeneration {
			e.stopped = true
		}
		e.mu.Unlock()
		return err
	}
	pty.setWriteGeneration(generation)

	e.mu.Lock()
	if lifecycle != e.lifecycleGeneration || generation != e.listenerGeneration || e.pty != nil {
		e.mu.Unlock()
		_ = pty.Close()
		return fmt.Errorf("start cancelled")
	}
	e.pty = pty
	e.mu.Unlock()

	current, err := e.resizeAttachedPTY(pty, generation)
	if !current {
		_ = pty.Close()
		return fmt.Errorf("start cancelled")
	}
	if err != nil {
		return fmt.Errorf("resize pty: %w", err)
	}
	return nil
}

// StartWithEnv begins spawning the PTY process with extra environment variables.
// A non-fatal initial resize failure is reported in PtyReadyMsg.ResizeErr.
func (e *Emulator) StartWithEnv(extraEnv []string) tea.Cmd {
	e.mu.Lock()
	e.lifecycleGeneration++
	lifecycle := e.lifecycleGeneration
	env := e.effectiveEnv(extraEnv)
	e.mu.Unlock()

	return func() tea.Msg {
		e.mu.Lock()
		if lifecycle != e.lifecycleGeneration {
			e.mu.Unlock()
			return nil
		}
		if e.pty != nil {
			generation := e.listenerGeneration
			sessionID := e.sessionID
			e.mu.Unlock()
			return PtyReadyMsg{SessionID: sessionID, Generation: generation, AlreadyRunning: true}
		}

		e.resetTerminalLocked()
		generation := e.listenerGeneration
		command := e.cmd
		args := append([]string(nil), e.args...)
		initialCWD := e.initialCWD
		sessionID := e.sessionID
		rows, cols := e.requestedHeight, e.requestedWidth
		e.mu.Unlock()

		pty, err := spawnPtyConfig(command, args, initialCWD, sessionID, rows, cols, env)
		if err != nil {
			return PtyExitMsg{SessionID: sessionID, Generation: generation, Err: err}
		}
		pty.setWriteGeneration(generation)

		e.mu.Lock()
		if lifecycle != e.lifecycleGeneration || generation != e.listenerGeneration || e.pty != nil {
			e.mu.Unlock()
			_ = pty.Close()
			return nil
		}
		e.pty = pty
		e.mu.Unlock()

		current, err := e.resizeAttachedPTY(pty, generation)
		if !current {
			_ = pty.Close()
			return nil
		}
		var resizeErr error
		if err != nil {
			resizeErr = fmt.Errorf("resize pty: %w", err)
		}
		return PtyReadyMsg{SessionID: sessionID, Generation: generation, ResizeErr: resizeErr}
	}
}

func (e *Emulator) resetTerminalLocked() {
	e.stopped = false
	e.cwd = WorkingDirectory{}
	e.cwdSet = false
	e.title = ""
	e.pendingCWDChanges = nil
	e.pendingTitleChanges = nil
	e.pendingResponses = nil
	e.listenerPending = false
	e.listenerGeneration++
	rows, cols := e.requestedHeight, e.requestedWidth
	if validateTerminalSize(rows, cols) != nil {
		rows, cols = defaultTerminalRows, defaultTerminalCols
		e.requestedHeight, e.requestedWidth = rows, cols
	}
	e.height, e.width = rows, cols
	e.screen = NewScreen(rows, cols)
	if e.scrollbackLimitSet {
		e.screen.SetScrollbackLimit(e.scrollbackLimit)
	}
	e.parser = NewParser(e.screen)
	e.parser.SetCWDCallback(func(cwd WorkingDirectory) {
		if e.cwdSet && cwd == e.cwd {
			return
		}
		e.cwd = cwd
		e.cwdSet = true
		e.pendingCWDChanges = append(e.pendingCWDChanges, cwd)
	})
	e.parser.SetTitleCallback(func(title string) {
		if title == e.title {
			return
		}
		e.title = title
		e.pendingTitleChanges = append(e.pendingTitleChanges, title)
	})
	e.parser.SetResponseCallback(func(data []byte) {
		e.pendingResponses = append(e.pendingResponses, append([]byte(nil), data...))
	})
}

func envValue(extraEnv []string, key string) (string, bool) {
	prefix := key + "="
	for i := len(extraEnv) - 1; i >= 0; i-- {
		if strings.HasPrefix(extraEnv[i], prefix) {
			return strings.TrimPrefix(extraEnv[i], prefix), true
		}
	}
	return os.LookupEnv(key)
}

func spawnPtyConfig(command string, args []string, initialCWD, sessionID string, rows, cols int, extraEnv []string) (*Pty, error) {
	env := append([]string{"AUTOMATA_SESSION_ID=" + sessionID}, extraEnv...)

	// Bash OSC 7 emitter. PWD is percent-encoded byte-by-byte before it is
	// placed inside the control sequence, so unusual filenames cannot inject
	// BEL/ESC or terminate the OSC payload. Preserve any caller PROMPT_COMMAND.
	osc7Cmd := `__o=$(LC_ALL=C;__p="$PWD";__r="";for ((__i=0;__i<${#__p};__i++));do __c=${__p:__i:1};case "$__c" in [a-zA-Z0-9/~._-])__r+="$__c";;*)printf -v __h '%%%02X' "'$__c";__r+="$__h";;esac;done;printf '%s' "$__r");printf '\033]7;file://localhost%s\007' "$__o";unset __o`
	if command == "/bin/bash" || command == "/usr/bin/bash" || strings.HasSuffix(command, "/bash") {
		if previous, ok := envValue(extraEnv, "PROMPT_COMMAND"); ok && strings.TrimSpace(previous) != "" {
			osc7Cmd += ";" + previous
		}
		env = append(env, "PROMPT_COMMAND="+osc7Cmd)
	}

	return spawnPtyWithSize(command, args, initialCWD, rows, cols, env...)
}

// Listen returns a command that waits for the next PTY output.
// At most one returned command may be waiting for a given Emulator.
func (e *Emulator) Listen() tea.Cmd {
	e.mu.Lock()
	if e.pty == nil || e.listenerPending {
		e.mu.Unlock()
		return nil
	}
	pty := e.pty
	generation := e.listenerGeneration
	e.listenerPending = true
	e.mu.Unlock()

	return func() tea.Msg {
		msg := pty.Listen(e.sessionID)()
		switch m := msg.(type) {
		case PtyOutputMsg:
			m.Generation = generation
			msg = m
		case PtyExitMsg:
			m.Generation = generation
			msg = m
		case PtyWarningMsg:
			m.Generation = generation
			msg = m
		}

		e.mu.Lock()
		if e.listenerGeneration == generation {
			e.listenerPending = false
		}
		e.mu.Unlock()
		return msg
	}
}

// DefaultShell returns a usable login shell, preferring absolute paths so it
// works even when the parent PTY provides a minimal or empty PATH.
func DefaultShell() (string, []string) {
	for _, shell := range []string{"/bin/bash", "/usr/bin/bash", "/bin/zsh", "/usr/bin/zsh", "/bin/sh", "/usr/bin/sh"} {
		if _, err := os.Stat(shell); err == nil {
			return shell, []string{"-l"}
		}
	}
	if _, err := exec.LookPath("bash"); err == nil {
		return "bash", []string{"-l"}
	}
	if _, err := exec.LookPath("sh"); err == nil {
		return "sh", []string{"-l"}
	}
	return "sh", nil
}

func defaultShell() (string, []string) {
	return DefaultShell()
}

// PtyState returns an immutable snapshot of the attached PTY.
func (e *Emulator) PtyState() PtyState {
	e.mu.RLock()
	pty := e.pty
	e.mu.RUnlock()
	if pty == nil {
		return PtyState{}
	}
	return pty.State()
}

// View renders the terminal screen at the given panel size. The screen size is
// kept in sync by ResizeMsg from the warp layout engine; during a drag the
// panel is filled by warp's padContent and the real resize happens on release.
func (e *Emulator) View(width, height int) string {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.stopped {
		return renderAsciiArt(width, height)
	}
	if e.screen == nil {
		return emptyView(width, height)
	}

	return e.screen.Render()
}

// Update handles messages.
func (e *Emulator) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return e.handleKey(msg)
	case tea.MouseMsg:
		return e.handleMouse(msg)
	case tea.WindowSizeMsg:
		return e.handleResize(msg)
	case ResizeMsg:
		return e.handlePanelResize(msg)
	case PtyReadyMsg:
		e.mu.RLock()
		generation := e.listenerGeneration
		onError := e.onError
		e.mu.RUnlock()
		if msg.SessionID != e.sessionID || msg.Generation != generation || msg.AlreadyRunning {
			return nil
		}
		if msg.ResizeErr != nil && onError != nil {
			onError(msg.ResizeErr)
		}
		return e.Listen()
	case tea.FocusMsg:
		return e.Focus()
	case tea.BlurMsg:
		return e.Blur()
	case CursorBlinkMsg:
		e.mu.Lock()
		if e.screen != nil {
			e.screen.markDirty()
			e.screen.cursorBlinkVisible = !e.screen.cursorBlinkVisible
		}
		e.mu.Unlock()
		return nil
	case PtyOutputMsg:
		e.mu.Lock()
		if msg.SessionID != e.sessionID || msg.Generation != e.listenerGeneration {
			e.mu.Unlock()
			return nil
		}
		parser := e.parser
		if parser != nil && len(msg.Data) > 0 {
			// Validate generation and mutate the parser under the same lock so a
			// Close/Start transition cannot redirect an old PTY chunk into a new
			// terminal instance.
			parser.Feed(msg.Data)
		}
		cwdChanges := append([]WorkingDirectory(nil), e.pendingCWDChanges...)
		e.pendingCWDChanges = nil
		titleChanges := append([]string(nil), e.pendingTitleChanges...)
		e.pendingTitleChanges = nil
		responses := append([][]byte(nil), e.pendingResponses...)
		e.pendingResponses = nil
		onCWDChange := e.onCWDChange
		onTitleChange := e.onTitleChange
		pty := e.pty
		generation := e.listenerGeneration
		e.mu.Unlock()
		if onCWDChange != nil {
			for _, cwd := range cwdChanges {
				onCWDChange(cwd)
			}
		}
		if onTitleChange != nil {
			for _, title := range titleChanges {
				onTitleChange(title)
			}
		}
		listen := e.Listen()
		if len(responses) == 0 || pty == nil {
			return listen
		}
		reply := func() tea.Msg {
			for _, data := range responses {
				if err := pty.writeForGeneration(generation, data); err != nil {
					return PtyExitMsg{SessionID: e.sessionID, Generation: generation, Err: err}
				}
			}
			return nil
		}
		if listen == nil {
			return reply
		}
		return tea.Sequence(reply, listen)
	case RenderTickMsg:
		// Legacy tick messages are intentionally ignored. They must not start a
		// second PTY listener or delay current output.
		return nil
	case PtyExitMsg:
		e.mu.Lock()
		if msg.SessionID != e.sessionID || msg.Generation != e.listenerGeneration {
			e.mu.Unlock()
			return nil
		}
		pty := e.pty
		if pty != nil {
			pty.invalidateWrites()
		}
		clipboardStore := e.clipboardTempStore
		e.clipboardTempStore = nil
		e.pty = nil
		e.listenerPending = false
		e.listenerGeneration++
		e.lifecycleGeneration++
		e.stopped = true
		onError := e.onError
		onExit := e.onExit
		e.mu.Unlock()
		if pty != nil {
			_ = pty.Close()
		}
		if clipboardStore != nil {
			if err := clipboardStore.close(); err != nil && onError != nil {
				onError(err)
			}
		}
		if msg.ProcessExited && onExit != nil {
			onExit(msg)
		}
		if msg.Err != nil && onError != nil {
			onError(msg.Err)
		}
		return nil
	case PtyWarningMsg:
		e.mu.RLock()
		current := msg.SessionID == e.sessionID && msg.Generation == e.listenerGeneration
		onError := e.onError
		e.mu.RUnlock()
		if !current {
			return nil
		}
		if msg.Err != nil && onError != nil {
			onError(msg.Err)
		}
		return e.Listen()
	case ClipboardErrorMsg:
		e.mu.RLock()
		onError := e.onError
		e.mu.RUnlock()
		if msg.Err != nil && onError != nil {
			onError(msg.Err)
		}
		return nil
	case PtyErrorMsg:
		e.mu.RLock()
		onError := e.onError
		e.mu.RUnlock()
		if msg.Err != nil && onError != nil {
			onError(msg.Err)
		}
		return nil
	}
	return nil
}

func (e *Emulator) handleKey(msg tea.KeyMsg) tea.Cmd {
	e.mu.Lock()

	// Any keystroke returns the view to the live screen.
	if e.screen != nil {
		e.screen.ResetView()
	}

	if e.pty == nil {
		e.mu.Unlock()
		return nil
	}
	modes := keyEncodingModes{}
	if e.screen != nil {
		modes.applicationCursor = e.screen.applicationCursor
		modes.bracketedPaste = e.screen.bracketedPaste
	}
	data := keyToBytesWithModes(msg, modes)
	if len(data) == 0 {
		e.mu.Unlock()
		return nil
	}

	// Capture the command line before sending Enter to the PTY.
	historyChanged := false
	if msg.Type == tea.KeyEnter && e.screen != nil {
		line := e.screen.LineText(e.screen.cursor.Row)
		cmd := stripPrompt(line)
		if cmd != "" && (len(e.commandHistory) == 0 || e.commandHistory[len(e.commandHistory)-1] != cmd) {
			e.commandHistory = append(e.commandHistory, cmd)
			historyChanged = true
			if len(e.commandHistory) > maxCommandHistory {
				e.commandHistory = e.commandHistory[len(e.commandHistory)-maxCommandHistory:]
			}
		}
	}

	// Copy callback state while locked, but never invoke external code under
	// the emulator mutex: callbacks may safely call back into Emulator.
	onHistoryChanged := e.onCommandHistoryChanged
	history := append([]string(nil), e.commandHistory...)
	pty := e.pty
	generation := e.listenerGeneration
	e.mu.Unlock()

	if onHistoryChanged != nil && historyChanged {
		onHistoryChanged(history)
	}

	return queueInteractivePtyWrite(pty, e.sessionID, generation, data)
}

func queueInteractivePtyWrite(pty *Pty, sessionID string, generation uint64, data []byte) tea.Cmd {
	result, err := pty.enqueueInteractiveWrite(generation, data)
	if err != nil {
		if err == errPtyClosed || err == errStalePtyWrite {
			return nil
		}
		return func() tea.Msg { return PtyErrorMsg{Err: err} }
	}
	if result == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case err := <-result:
			if err != nil && err != errPtyClosed && err != errStalePtyWrite {
				return PtyExitMsg{SessionID: sessionID, Generation: generation, Err: err}
			}
		case <-pty.done:
		}
		return nil
	}
}

// stripPrompt removes the shell prompt prefix from a terminal line.
// It looks for the last occurrence of common prompt ending characters.
func stripPrompt(line string) string {
	// Find the last prompt marker: $, #, >, or %.
	best := -1
	for _, ch := range []string{"$ ", "# ", "> ", "% "} {
		if idx := strings.LastIndex(line, ch); idx >= 0 && idx+len(ch) > best {
			best = idx + len(ch)
		}
	}
	if best < 0 {
		return strings.TrimSpace(line)
	}
	return strings.TrimSpace(line[best:])
}

func isMouseButtonDown(button tea.MouseButton) bool {
	switch button {
	case tea.MouseButtonLeft, tea.MouseButtonMiddle, tea.MouseButtonRight,
		tea.MouseButtonBackward, tea.MouseButtonForward, tea.MouseButton10, tea.MouseButton11:
		return true
	default:
		return false
	}
}

func shouldReportMouse(mode int, msg tea.MouseMsg) bool {
	if mode == 0 {
		return false
	}
	if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown ||
		msg.Button == tea.MouseButtonWheelLeft || msg.Button == tea.MouseButtonWheelRight {
		return true
	}
	switch msg.Action {
	case tea.MouseActionPress, tea.MouseActionRelease:
		return true
	case tea.MouseActionMotion:
		return mode == 1003 || (mode == 1002 && isMouseButtonDown(msg.Button))
	default:
		return false
	}
}

func (e *Emulator) handleMouse(msg tea.MouseMsg) tea.Cmd {
	e.mu.Lock()
	screen := e.screen
	pty := e.pty
	generation := e.listenerGeneration
	mode := 0
	sgr := false
	if screen != nil {
		mode = screen.mouseTrackingMode()
		sgr = screen.mouseSGR
	}

	// Shift forces local terminal selection/scrolling even when the child has
	// enabled mouse reporting, matching conventional terminal behaviour.
	reportToChild := mode != 0 && !msg.Shift
	if e.dragSelecting && msg.Action == tea.MouseActionRelease {
		reportToChild = false
	}
	if reportToChild {
		e.mu.Unlock()
		if pty == nil || !shouldReportMouse(mode, msg) {
			return nil
		}
		data := mouseToBytes(msg, sgr)
		if len(data) == 0 {
			return nil
		}
		return queueInteractivePtyWrite(pty, e.sessionID, generation, data)
	}

	if screen == nil {
		e.mu.Unlock()
		return nil
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		screen.ScrollViewUp(3)
		e.mu.Unlock()
		return nil
	case tea.MouseButtonWheelDown:
		screen.ScrollViewDown(3)
		e.mu.Unlock()
		return nil
	}

	var copyLines []string
	var copyErr error
	switch msg.Action {
	case tea.MouseActionPress:
		if msg.Button == tea.MouseButtonLeft {
			e.pressX, e.pressY = msg.X, msg.Y
			e.dragSelecting = false
		}
	case tea.MouseActionMotion:
		if msg.Button == tea.MouseButtonLeft {
			if !e.dragSelecting {
				dx := msg.X - e.pressX
				if dx < 0 {
					dx = -dx
				}
				dy := msg.Y - e.pressY
				if dy < 0 {
					dy = -dy
				}
				if dx > 0 || dy > 0 {
					screen.StartSelection(e.pressY, e.pressX)
					e.dragSelecting = true
				}
			}
			if e.dragSelecting {
				screen.ExtendSelection(msg.Y, msg.X)
			}
		}
	case tea.MouseActionRelease:
		if msg.Button == tea.MouseButtonLeft || msg.Button == tea.MouseButtonNone {
			if e.dragSelecting {
				copyLines, copyErr = screen.selectionTextWithLimit(maxClipboardBytes, maxClipboardCells)
				screen.ClearSelection()
			}
			e.dragSelecting = false
		}
	}
	e.mu.Unlock()

	if copyErr != nil {
		return func() tea.Msg { return ClipboardErrorMsg{Err: copyErr} }
	}
	if len(copyLines) == 0 {
		return nil
	}
	return func() tea.Msg {
		if err := copyToClipboard(copyLines); err != nil {
			return ClipboardErrorMsg{Err: err}
		}
		return nil
	}
}

// mouseToBytes encodes a Bubble Tea mouse event using either SGR extended
// coordinates (?1006) or the legacy X10 encoding.
func mouseToBytes(msg tea.MouseMsg, sgr bool) []byte {
	if msg.X < 0 || msg.Y < 0 {
		return nil
	}

	var cb int
	switch msg.Button {
	case tea.MouseButtonLeft:
		cb = 0
	case tea.MouseButtonMiddle:
		cb = 1
	case tea.MouseButtonRight:
		cb = 2
	case tea.MouseButtonWheelUp:
		cb = 64
	case tea.MouseButtonWheelDown:
		cb = 65
	case tea.MouseButtonWheelLeft:
		cb = 66
	case tea.MouseButtonWheelRight:
		cb = 67
	case tea.MouseButtonBackward:
		cb = 128
	case tea.MouseButtonForward:
		cb = 129
	case tea.MouseButton10:
		cb = 130
	case tea.MouseButton11:
		cb = 131
	default:
		cb = 3
	}

	if !sgr && msg.Action == tea.MouseActionRelease {
		cb = 3
	}
	if msg.Action == tea.MouseActionMotion {
		cb |= 0b0010_0000
	}
	if msg.Shift {
		cb |= 0b0000_0100
	}
	if msg.Alt {
		cb |= 0b0000_1000
	}
	if msg.Ctrl {
		cb |= 0b0001_0000
	}

	x := msg.X + 1
	y := msg.Y + 1
	if sgr {
		suffix := 'M'
		if msg.Action == tea.MouseActionRelease {
			suffix = 'm'
		}
		return []byte(fmt.Sprintf("\x1b[<%d;%d;%d%c", cb, x, y, suffix))
	}

	// Legacy X10 encoding supports coordinates only through 223.
	if x > 223 || y > 223 || cb > 223 {
		return nil
	}
	return []byte{0x1b, '[', 'M', byte(cb + 32), byte(x + 32), byte(y + 32)}
}

func (e *Emulator) handleResize(msg tea.WindowSizeMsg) tea.Cmd {
	// WindowSizeMsg carries the full window size; panel size comes via ResizeMsg.
	return nil
}

func (e *Emulator) handlePanelResize(msg ResizeMsg) tea.Cmd {
	if err := validateTerminalSize(msg.Height, msg.Width); err != nil {
		return nil
	}
	e.mu.Lock()
	if err := e.requestTerminalSizeLocked(msg.Height, msg.Width); err != nil {
		e.mu.Unlock()
		return nil
	}
	pty := e.pty
	generation := e.listenerGeneration
	if pty == nil {
		if err := e.applyTerminalSizeLocked(msg.Height, msg.Width); err != nil {
			e.mu.Unlock()
			return nil
		}
		e.mu.Unlock()
		return nil
	}
	e.mu.Unlock()

	current, err := e.resizeAttachedPTY(pty, generation)
	if err != nil {
		return func() tea.Msg {
			return PtyWarningMsg{SessionID: e.sessionID, Generation: generation, Err: fmt.Errorf("resize pty: %w", err)}
		}
	}
	if !current {
		return nil
	}
	return nil
}

func (e *Emulator) requestTerminalSizeLocked(rows, cols int) error {
	if err := validateTerminalSize(rows, cols); err != nil {
		return err
	}
	if e.requestedHeight == rows && e.requestedWidth == cols {
		return nil
	}
	e.requestedHeight = rows
	e.requestedWidth = cols
	e.resizeGeneration++
	return nil
}

func (e *Emulator) applyTerminalSizeLocked(rows, cols int) error {
	if err := validateTerminalSize(rows, cols); err != nil {
		return err
	}
	if e.height == rows && e.width == cols {
		return nil
	}
	if e.screen != nil {
		if err := e.screen.ResizeChecked(rows, cols); err != nil {
			return err
		}
	}
	e.height = rows
	e.width = cols
	return nil
}

func (e *Emulator) resizeAttachedPTY(pty *Pty, generation uint64) (bool, error) {
	e.resizeMu.Lock()
	defer e.resizeMu.Unlock()

	for {
		e.mu.RLock()
		if e.pty != pty || e.listenerGeneration != generation {
			e.mu.RUnlock()
			return false, nil
		}
		rows, cols := e.requestedHeight, e.requestedWidth
		resizeGeneration := e.resizeGeneration
		e.mu.RUnlock()

		if err := validateTerminalSize(rows, cols); err != nil {
			return true, err
		}
		if err := pty.Resize(rows, cols); err != nil {
			e.mu.RLock()
			current := e.pty == pty && e.listenerGeneration == generation
			changed := e.resizeGeneration != resizeGeneration
			e.mu.RUnlock()
			if !current {
				return false, nil
			}
			if changed {
				continue
			}
			return true, err
		}

		e.mu.Lock()
		if e.pty != pty || e.listenerGeneration != generation {
			e.mu.Unlock()
			return false, nil
		}
		if err := e.applyTerminalSizeLocked(rows, cols); err != nil {
			e.mu.Unlock()
			return true, err
		}
		changed := e.resizeGeneration != resizeGeneration
		e.mu.Unlock()
		if !changed {
			return true, nil
		}
	}
}

func (e *Emulator) focusChanged(focused bool) tea.Cmd {
	e.mu.Lock()
	e.focused = focused
	pty := e.pty
	generation := e.listenerGeneration
	report := e.screen != nil && e.screen.focusReporting
	e.mu.Unlock()
	if !report || pty == nil {
		return nil
	}
	data := []byte("\x1b[O")
	if focused {
		data = []byte("\x1b[I")
	}
	return func() tea.Msg {
		if err := pty.writeForGeneration(generation, data); err != nil {
			return PtyExitMsg{SessionID: e.sessionID, Generation: generation, Err: err}
		}
		return nil
	}
}

// Focus marks the emulator as focused and returns a command that reports focus
// to the child when DECSET ?1004 is active.
func (e *Emulator) Focus() tea.Cmd {
	return e.focusChanged(true)
}

// Blur marks the emulator as blurred and returns a command that reports blur
// to the child when DECSET ?1004 is active.
func (e *Emulator) Blur() tea.Cmd {
	return e.focusChanged(false)
}

// Close closes the PTY and removes this Emulator's private clipboard files.
func (e *Emulator) Close() error {
	e.mu.Lock()
	pty := e.pty
	if pty != nil {
		pty.invalidateWrites()
	}
	clipboardStore := e.clipboardTempStore
	e.clipboardTempStore = nil
	e.pty = nil
	e.listenerPending = false
	e.listenerGeneration++
	e.lifecycleGeneration++
	e.mu.Unlock()
	var closeErr error
	if pty != nil {
		closeErr = pty.Close()
	}
	if clipboardStore != nil {
		if err := clipboardStore.close(); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}

// Stop terminates the session, removes clipboard files, and shows the idle view.
func (e *Emulator) Stop() error {
	e.mu.Lock()
	pty := e.pty
	if pty != nil {
		pty.invalidateWrites()
	}
	clipboardStore := e.clipboardTempStore
	e.clipboardTempStore = nil
	e.pty = nil
	e.listenerPending = false
	e.listenerGeneration++
	e.lifecycleGeneration++
	e.stopped = true
	e.mu.Unlock()
	var closeErr error
	if pty != nil {
		closeErr = pty.Close()
	}
	if clipboardStore != nil {
		if err := clipboardStore.close(); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}

// SetInitialCWD sets the directory in which the PTY process starts.
func (e *Emulator) SetInitialCWD(dir string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initialCWD = dir
}

// CurrentWorkingDirectory returns the last OSC 7 location, falling back to the
// configured initial directory. The boolean is false when neither is known.
func (e *Emulator) CurrentWorkingDirectory() (WorkingDirectory, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.cwdSet {
		return e.cwd, true
	}
	if e.initialCWD != "" {
		return WorkingDirectory{Path: e.initialCWD, Local: true}, true
	}
	return WorkingDirectory{}, false
}

// CWD returns the directory path without authority metadata.
// Use CurrentWorkingDirectory to distinguish local and remote locations.
func (e *Emulator) CWD() string {
	cwd, ok := e.CurrentWorkingDirectory()
	if !ok {
		return ""
	}
	return cwd.Path
}

// Title returns the last validated terminal title reported through OSC 0/2.
func (e *Emulator) Title() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.title
}

// CommandHistorySnapshot returns a copy of the command list. New captures are
// heuristically scraped from the visible prompt line, not read from shell history.
func (e *Emulator) CommandHistorySnapshot() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]string(nil), e.commandHistory...)
}

// SetCommandHistory restores a previously saved heuristic command list, keeping
// only the newest maxCommandHistory entries.
func (e *Emulator) SetCommandHistory(history []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(history) > maxCommandHistory {
		history = history[len(history)-maxCommandHistory:]
	}
	e.commandHistory = append([]string(nil), history...)
}

func renderAsciiArt(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(asciiArtIcon, "\n"), "\n")
	artH := len(lines)
	artW := 0
	for _, l := range lines {
		if w := lipgloss.Width(l); w > artW {
			artW = w
		}
	}

	startY := (height - artH) / 2
	if startY < 0 {
		startY = 0
	}
	startX := (width - artW) / 2
	if startX < 0 {
		startX = 0
	}

	var out []string
	for y := 0; y < height; y++ {
		if y >= startY && y-startY < artH {
			line := lines[y-startY]
			pad := startX
			lineW := lipgloss.Width(line)
			if pad+lineW > width {
				line = truncateToWidth(line, width-pad)
				lineW = lipgloss.Width(line)
			}
			trail := width - pad - lineW
			if trail < 0 {
				trail = 0
			}
			out = append(out, strings.Repeat(" ", pad)+line+strings.Repeat(" ", trail))
		} else {
			out = append(out, strings.Repeat(" ", width))
		}
	}
	return strings.Join(out, "\n")
}

func truncateToWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		candidate := b.String() + string(r)
		if lipgloss.Width(candidate) > width {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

func emptyView(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	line := ""
	for i := 0; i < width; i++ {
		line += " "
	}
	var lines []string
	for i := 0; i < height; i++ {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

type keyEncodingModes struct {
	applicationCursor bool
	bracketedPaste    bool
}

func keyToBytes(msg tea.KeyMsg) []byte {
	return keyToBytesWithModes(msg, keyEncodingModes{})
}

func keyToBytesWithModes(msg tea.KeyMsg, modes keyEncodingModes) []byte {
	if msg.Type == tea.KeyRunes {
		data := []byte(string(msg.Runes))
		if msg.Paste && modes.bracketedPaste {
			data = append([]byte("\x1b[200~"), data...)
			data = append(data, []byte("\x1b[201~")...)
			return data
		}
		return withAltPrefix(data, msg.Alt)
	}

	if msg.Type >= tea.KeyCtrlAt && msg.Type <= tea.KeyCtrlUnderscore || msg.Type == tea.KeyBackspace {
		return withAltPrefix([]byte{byte(msg.Type)}, msg.Alt)
	}

	shift, ctrl := keyModifiers(msg.Type)
	modifier := xtermModifier(shift, msg.Alt, ctrl)

	switch msg.Type {
	case tea.KeyUp, tea.KeyShiftUp, tea.KeyCtrlUp, tea.KeyCtrlShiftUp:
		return cursorKeySequence('A', modifier, modes.applicationCursor)
	case tea.KeyDown, tea.KeyShiftDown, tea.KeyCtrlDown, tea.KeyCtrlShiftDown:
		return cursorKeySequence('B', modifier, modes.applicationCursor)
	case tea.KeyRight, tea.KeyShiftRight, tea.KeyCtrlRight, tea.KeyCtrlShiftRight:
		return cursorKeySequence('C', modifier, modes.applicationCursor)
	case tea.KeyLeft, tea.KeyShiftLeft, tea.KeyCtrlLeft, tea.KeyCtrlShiftLeft:
		return cursorKeySequence('D', modifier, modes.applicationCursor)
	case tea.KeyHome, tea.KeyShiftHome, tea.KeyCtrlHome, tea.KeyCtrlShiftHome:
		return cursorKeySequence('H', modifier, modes.applicationCursor)
	case tea.KeyEnd, tea.KeyShiftEnd, tea.KeyCtrlEnd, tea.KeyCtrlShiftEnd:
		return cursorKeySequence('F', modifier, modes.applicationCursor)
	case tea.KeyPgUp, tea.KeyCtrlPgUp:
		return tildeKeySequence(5, modifier)
	case tea.KeyPgDown, tea.KeyCtrlPgDown:
		return tildeKeySequence(6, modifier)
	case tea.KeyDelete:
		return tildeKeySequence(3, modifier)
	case tea.KeyInsert:
		return tildeKeySequence(2, modifier)
	case tea.KeyShiftTab:
		return []byte("\x1b[Z")
	case tea.KeySpace:
		return withAltPrefix([]byte(" "), msg.Alt)
	case tea.KeyF1, tea.KeyF2, tea.KeyF3, tea.KeyF4:
		final := byte('P' + (tea.KeyF1 - msg.Type))
		if modifier == 1 {
			return []byte{0x1b, 'O', final}
		}
		return []byte(fmt.Sprintf("\x1b[1;%d%c", modifier, final))
	case tea.KeyF5, tea.KeyF6, tea.KeyF7, tea.KeyF8, tea.KeyF9, tea.KeyF10, tea.KeyF11, tea.KeyF12:
		codes := [...]int{15, 17, 18, 19, 20, 21, 23, 24}
		return tildeKeySequence(codes[int(tea.KeyF5-msg.Type)], modifier)
	case tea.KeyF13, tea.KeyF14, tea.KeyF15, tea.KeyF16:
		return []byte(fmt.Sprintf("\x1b[1;2%c", byte('P'+(tea.KeyF13-msg.Type))))
	case tea.KeyF17, tea.KeyF18, tea.KeyF19, tea.KeyF20:
		codes := [...]int{15, 17, 18, 19}
		return []byte(fmt.Sprintf("\x1b[%d;2~", codes[int(tea.KeyF17-msg.Type)]))
	}
	return nil
}

func withAltPrefix(data []byte, alt bool) []byte {
	if !alt {
		return data
	}
	result := make([]byte, 1, len(data)+1)
	result[0] = 0x1b
	return append(result, data...)
}

func keyModifiers(key tea.KeyType) (shift, ctrl bool) {
	switch key {
	case tea.KeyShiftUp, tea.KeyShiftDown, tea.KeyShiftRight, tea.KeyShiftLeft,
		tea.KeyShiftHome, tea.KeyShiftEnd:
		shift = true
	case tea.KeyCtrlUp, tea.KeyCtrlDown, tea.KeyCtrlRight, tea.KeyCtrlLeft,
		tea.KeyCtrlHome, tea.KeyCtrlEnd, tea.KeyCtrlPgUp, tea.KeyCtrlPgDown:
		ctrl = true
	case tea.KeyCtrlShiftUp, tea.KeyCtrlShiftDown, tea.KeyCtrlShiftRight, tea.KeyCtrlShiftLeft,
		tea.KeyCtrlShiftHome, tea.KeyCtrlShiftEnd:
		shift = true
		ctrl = true
	}
	return shift, ctrl
}

func xtermModifier(shift, alt, ctrl bool) int {
	modifier := 1
	if shift {
		modifier++
	}
	if alt {
		modifier += 2
	}
	if ctrl {
		modifier += 4
	}
	return modifier
}

func cursorKeySequence(final byte, modifier int, application bool) []byte {
	if modifier != 1 {
		return []byte(fmt.Sprintf("\x1b[1;%d%c", modifier, final))
	}
	if application {
		return []byte{0x1b, 'O', final}
	}
	return []byte{0x1b, '[', final}
}

func tildeKeySequence(code, modifier int) []byte {
	if modifier == 1 {
		return []byte(fmt.Sprintf("\x1b[%d~", code))
	}
	return []byte(fmt.Sprintf("\x1b[%d;%d~", code, modifier))
}

// CursorBlinkMsg is sent by the host to toggle the cursor blink state.
// A single host-level timer should broadcast this message to all visible
// terminal emulators so their cursors blink in sync.
type CursorBlinkMsg struct{}

// PtyReadyMsg is sent when the PTY is ready for listening. ResizeErr is a
// non-fatal error applying a requested PTY size after the process started.
type PtyReadyMsg struct {
	SessionID      string
	Generation     uint64
	AlreadyRunning bool
	ResizeErr      error
}
