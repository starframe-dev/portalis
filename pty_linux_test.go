//go:build linux

package portalis

import (
	"strings"
	"testing"
)

func TestLinuxPTYEIOIsNormalExit(t *testing.T) {
	p, err := Spawn("/bin/sh", []string{"-c", "printf hello; exit 0"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	var output strings.Builder
	for {
		msg := p.Listen("linux-eio")()
		switch msg := msg.(type) {
		case PtyOutputMsg:
			output.Write(msg.Data)
		case PtyExitMsg:
			if got := output.String(); !strings.Contains(got, "hello") {
				t.Fatalf("final output = %q, want hello", got)
			}
			if msg.Err != nil {
				t.Fatalf("normal Linux PTY shutdown reported error: %v", msg.Err)
			}
			if !msg.ProcessExited || msg.ExitCode != 0 || msg.Signal != nil {
				t.Fatalf("normal exit = %#v, want exited code 0 without signal", msg)
			}
			return
		default:
			t.Fatalf("unexpected PTY message %T", msg)
		}
	}
}
