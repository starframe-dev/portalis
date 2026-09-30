# Portalis

A built-in terminal emulator for Go. Runs a shell in a PTY, parses ANSI
output, and renders the result through [Bubble Tea](https://github.com/charmbracelet/bubbletea).
Designed to be embedded into a host application that allocates a rectangle
and forwards keyboard, mouse and resize events.

## Features

- PTY-backed session via [`creack/pty`](https://github.com/creack/pty)
- Bounded ANSI/VT subset: CSI, OSC, SGR colors (16 / 256 / 24-bit), UTF-8
- Child processes default to `TERM=ansi` (8-color terminfo), not xterm-256color; callers can override `TERM`
- xterm-compatible key encoding (Ctrl/Alt/Shift/F-keys, application cursor mode)
- DEC modes: `?1` application cursor, `?6` origin, `?7` autowrap, `?25` cursor visibility, `?1000/1002/1003/1006` mouse, `?1004` focus, `?1047/1048/1049` alternate buffer/cursor, `?2004` bracketed paste, `?2026` synchronized output
- Editing sequences: insert mode, tab stops, ICH (`CSI @`), DCH (`CSI P`), ECH (`CSI X`), IL (`CSI L`), DL (`CSI M`), SU (`CSI S`), SD (`CSI T`), VPA (`CSI d`), HPA (`CSI G`)
- RIS/DECSTR resets, scroll regions, index/reverse index, DEC Special Graphics charset
- OSC 0/2 title tracking and structured OSC 7 locations (`Host`, `Path`, `Local`)
- System clipboard selection and explicit paste integration (macOS, Wayland, X11)
- Synchronized output (`CSI ? 2026 h/l`)
- Mouse-drag selection, DEC mouse reporting, focus reporting, and host-driven cursor blinking
- Scrollback capped at 10 000 lines and 1 048 576 cells; alternate screen and bracketed paste
- Heuristic command capture from the visible prompt line (not shell history), capped at 1 000 entries
- Interactive key/mouse writes are admitted without waiting for PTY I/O; a shared FIFO writer preserves payload order
- Render dirty-cache and ordered 4 KiB PTY reads for responsive streaming
- Embeddable Bubble Tea component: the host owns its model, forwards messages and calls `View(w, h)` to render

## Installation

```bash
go get github.com/starframe-dev/portalis
```

Requires Go 1.25.8 or later. PTY process support currently targets Unix-like systems (Linux and macOS); Windows is not supported.

## Quick start

`Emulator` is an embeddable terminal component, **not** a `tea.Model` by
itself. A host model forwards Bubble Tea messages, sends the allocated content
size as `ResizeMsg`, and renders with `View(width, height)`. Resize errors are
reported through `SetOnError` without stopping the PTY; the screen keeps its last
successfully applied size:

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
    term.SetOnError(func(err error) { log.Printf("terminal: %v", err) })
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
    if _, err := tea.NewProgram(
        newModel(),
        tea.WithAltScreen(),
        tea.WithMouseCellMotion(),
        tea.WithReportFocus(),
    ).Run(); err != nil {
        log.Fatal(err)
    }
}
```

The returned commands produce `PtyReadyMsg`, `PtyOutputMsg`,
`PtyExitMsg`, clipboard errors, and other internal messages; route those
messages back through `Emulator.Update`. System clipboard paste is explicit
via `PasteFromClipboard()`; ordinary `Ctrl+V` is forwarded to the child. The
Quick Start enables mouse-cell motion for drag selection and focus messages for
child focus reporting. Portalis does not create a cursor timer: a host that wants
blinking should broadcast `portalis.CursorBlinkMsg{}` from one shared timer.

## Resource limits and diagnostics

- A terminal grid is limited to 262 144 cells; scrollback has a separate
  1 048 576-cell cap, even if its line-count limit is disabled. Grapheme payloads
  are capped at 64 bytes per cell (about 96 MiB worst-case across primary,
  alternate and scrollback buffers, excluding runtime/cell overhead).
- Clipboard subprocesses time out after 5 seconds. Clipboard text/image bytes
  are capped at 100 MiB; PNGs are limited to 25 million pixels and 128 MiB of
  worst-case decoded RGBA64 data. Per-emulator clipboard temp storage defaults
  to 16 files, 256 MiB total, and a one-hour TTL in a private `0700` directory.
- Bulk PTY writes are bounded to 100 MiB and 64 queued requests; interactive
  writes have a separate 64 KiB reserve and fail fast under overload.
- Setting `PORTALIS_RAW_TRACE=<base>` writes `<base>.<pid>` and a `.chunks`
  sidecar with mode `0600`. Each trace defaults to 16 MiB per file and 3 retained
  files; `PORTALIS_RAW_TRACE_MAX_BYTES` and `PORTALIS_RAW_TRACE_MAX_FILES` can
  tune those bounded limits. Trace warnings go to `SetOnError`. Traces include
  child output and may contain secrets; enable only for diagnostics.

## API migration

The API was tightened before v1: mutable `Screen` and `Pty` fields are private,
`Emulator.Pty()` was removed, callbacks use setters, and OSC 7 callbacks now
receive `WorkingDirectory`. See [MIGRATION.md](MIGRATION.md) for before/after
examples and the command-history limitation.

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
go vet ./...
staticcheck ./...
govulncheck ./...
go test -run=^$ -fuzz=FuzzParserFeed -fuzztime=5s .
```

CI also runs Linux PTY lifecycle tests and Linux/ARM64 cross-compilation; manual
and `v*` tag-triggered verification cross-compiles test binaries for Linux/macOS
on amd64/arm64. No workflow creates or publishes a release.
For visual TUI integration, install `cuetty-cli` and run
`go test -tags cuetty -run '^TestANSIStressCueTTY$' .`; it regenerates the
ignored `cuetty-artifacts/ansi-stress/` outputs. See
[`specs/ansi-stress-cue-tty.md`](specs/ansi-stress-cue-tty.md). The live Pi test
requires an authenticated Pi installation and is intentionally not part of CI.
`clipboard_mac_test.go` live tests are macOS-only and skipped by default; they replace
the system clipboard, so run them only explicitly with
`PORTALIS_RUN_CLIPBOARD_INTEGRATION=1`.
Other unit tests are portable.

## License

MIT (see project root).
