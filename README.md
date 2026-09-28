# Portalis

A built-in terminal emulator for Go. Runs a shell in a PTY, parses ANSI
output, and renders the result through [Bubble Tea](https://github.com/charmbracelet/bubbletea).
Designed to be embedded into a host application that allocates a rectangle
and forwards keyboard, mouse and resize events.

## Features

- PTY-backed session via [`creack/pty`](https://github.com/creack/pty)
- ANSI/VT parser: CSI, OSC, SGR colors (16 / 256 / 24-bit), UTF-8
- xterm-compatible key encoding (Ctrl/Alt/Shift/F-keys, application cursor mode)
- DEC modes: `?1` application cursor, `?6` origin, `?7` autowrap, `?25` cursor visibility, `?1000/1002/1003/1006` mouse, `?1004` focus, `?1049` alt screen, `?2004` bracketed paste, `?2026` synchronized output
- Editing sequences: ICH (`CSI @`), DCH (`CSI P`), ECH (`CSI X`), IL (`CSI L`), DL (`CSI M`), SU (`CSI S`), SD (`CSI T`), VPA (`CSI d`), HPA (`CSI G`)
- Scroll regions, index/reverse index, DEC Special Graphics charset
- OSC 7 working-directory tracking with callbacks
- System clipboard selection and explicit paste integration (macOS, Wayland, X11)
- Synchronized output (`CSI ? 2026 h/l`)
- Selection with mouse drag, scrollback up to 10 000 lines
- Alt screen, bracketed paste, command history
- Render dirty-cache and ordered 4 KiB PTY reads for responsive streaming
- Framework-agnostic core: feed events, call `View(w, h)` to render

## Installation

```bash
go get github.com/starframe-dev/portalis
```

Requires Go 1.25.8 or later. PTY process support currently targets Unix-like systems (Linux and macOS); Windows is not supported.

## Quick start

`Emulator` is an embeddable terminal component, **not** a `tea.Model` by
itself. A host model forwards Bubble Tea messages, sends the allocated content
size as `ResizeMsg`, and renders with `View(width, height)`:

```go
package main

import (
    "log"

    tea "github.com/charmbracelet/bubbletea"
    "github.com/starframe-dev/portalis"
)

type model struct {
    term *portalis.Emulator
    w, h int
}

func newModel() *model {
    term := portalis.NewEmulator("session-1", "build", "bash", []string{"-l"})
    term.OnError = func(err error) { log.Printf("terminal: %v", err) }
    return &model{term: term}
}

func (m *model) Init() tea.Cmd {
    return m.term.Start()
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
    switch msg := msg.(type) {
    case tea.WindowSizeMsg:
        m.w, m.h = msg.Width, msg.Height
        return m, m.term.Update(portalis.ResizeMsg{Width: m.w, Height: m.h})
    default:
        return m, m.term.Update(msg)
    }
}

func (m *model) View() string {
    return m.term.View(m.w, m.h)
}

func main() {
    if _, err := tea.NewProgram(newModel(), tea.WithAltScreen()).Run(); err != nil {
        log.Fatal(err)
    }
}
```

The returned commands produce `PtyReadyMsg`, `PtyOutputMsg`,
`PtyExitMsg`, clipboard errors, and other internal messages; route those
messages back through `Emulator.Update`. System clipboard paste is explicit
via `PasteFromClipboard()`; ordinary `Ctrl+V` is forwarded to the child.

## Architecture

| File | Responsibility |
|---|---|
| `emulator.go` | Top-level controller. Coordinates Screen, Parser and PTY. |
| `screen.go` | 2D cell grid, scrollback, selection, rendering, dirty cache. |
| `ansi.go` | ANSI/VT escape parser (CSI, OSC, SGR, UTF-8, DEC modes). |
| `pty.go` | PTY spawn / ordered reads / write / resize / lifecycle. |
| `clipboard.go` | System clipboard backends and explicit paste/copy integration. |

```
┌────────────────── Emulator ──────────────────┐
│  Parser  ──▶  Screen  ◀──  PTY (process I/O) │
└──────────────────────────────────────────────┘
```

## Documentation

Project specs live under `specs/` and per-file specs under `code-specs/`:

- [`specs/portalis.md`](specs/portalis.md) — full architecture overview
- [`code-specs/emulator.md`](code-specs/emulator.md) — public API of `Emulator`
- [`code-specs/screen.md`](code-specs/screen.md) — `Screen` cell grid
- [`code-specs/ansi.md`](code-specs/ansi.md) — parser states
- [`code-specs/pty.md`](code-specs/pty.md) — PTY lifecycle
- [`code-specs/clipboard.md`](code-specs/clipboard.md) — clipboard backends

## Testing

```bash
go test ./...
go test -race ./...
```

Unit tests cover ANSI sequences, screen operations, key encoding, and PTY resize/ordered reads. Visual E2E with `tmux` is run via `cuetty-cli` (see `cuetty-artifacts/portalis-tmux/`).

Note: `clipboard_mac_test.go` runs only on macOS; other tests are portable.

## License

MIT (see project root).
