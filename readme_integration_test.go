package portalis_test

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/starframe-dev/portalis"
)

// readmeHost mirrors the public integration pattern documented in README.
// Its purpose is compile-time protection against accidental API drift.
type readmeHost struct {
	term *portalis.Emulator
	w, h int
}

func newReadmeHost() *readmeHost {
	return &readmeHost{
		term: portalis.NewEmulator("session-1", "build", "bash", []string{"-l"}),
	}
}

func (m *readmeHost) Init() tea.Cmd {
	return m.term.Start()
}

func (m *readmeHost) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, m.term.Update(portalis.ResizeMsg{Width: m.w, Height: m.h})
	default:
		return m, m.term.Update(msg)
	}
}

func (m *readmeHost) View() string {
	return m.term.View(m.w, m.h)
}

var _ tea.Model = (*readmeHost)(nil)

func TestReadmeHostIntegrationCompiles(t *testing.T) {
	_ = tea.WithAltScreen()
	_ = tea.WithMouseCellMotion()
	_ = tea.WithReportFocus()
	host := newReadmeHost()
	if host.term == nil {
		t.Fatal("README host created nil emulator")
	}
}
