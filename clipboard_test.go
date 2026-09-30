package portalis

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestJoinClipboardLinesEnforcesByteLimit(t *testing.T) {
	got, err := joinClipboardLines([]string{"ab", "cd"}, 5)
	if err != nil || got != "ab\ncd" {
		t.Fatalf("bounded clipboard text = %q, %v; want ab\\ncd", got, err)
	}
	if _, err := joinClipboardLines([]string{"ab", "cd"}, 4); err == nil {
		t.Fatal("oversized clipboard text was accepted")
	}
}

func TestCommandOutputLimitedEnforcesBudgetAndTimeout(t *testing.T) {
	got, err := commandOutputLimitedWithLimit("", 4, time.Second, "/bin/sh", "-c", "printf 1234")
	if err != nil || string(got) != "1234" {
		t.Fatalf("bounded command output = %q, %v; want 1234", got, err)
	}
	if _, err := commandOutputLimitedWithLimit("", 4, time.Second, "/bin/sh", "-c", "printf 12345"); err == nil {
		t.Fatal("command output over byte budget was accepted")
	}
	if _, err := commandOutputLimitedWithLimit("", 10, 30*time.Millisecond, "/bin/sh", "-c", "exec sleep 2"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v, want timed out", err)
	}
}

func privateClipboardTestDir(t *testing.T) string {
	t.Helper()
	store, err := newClipboardTempStore(DefaultClipboardTempPolicy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	return store.directory()
}

func TestClipboardImageSwiftScriptUsesConfiguredByteLimit(t *testing.T) {
	script := clipboardImageSwiftScript()
	if strings.Contains(script, "__MAX_CLIPBOARD_BYTES__") || !strings.Contains(script, strconv.Itoa(maxClipboardBytes)) || !strings.Contains(script, "posixPermissions") || !strings.Contains(script, "CommandLine.arguments[1]") {
		t.Fatalf("Swift reader does not use configured clipboard limit")
	}
}

func TestParseClipboardImageSwiftOutput(t *testing.T) {
	if path, err := parseClipboardImageSwiftOutput([]byte("NO_IMAGE\n")); err != nil || path != "" {
		t.Fatalf("NO_IMAGE result = %q, %v", path, err)
	}
	if path, err := parseClipboardImageSwiftOutput([]byte("PATH:/tmp/image.png\n")); err != nil || path != "/tmp/image.png" {
		t.Fatalf("PATH result = %q, %v", path, err)
	}
	if _, err := parseClipboardImageSwiftOutput([]byte("ERR:clipboard image too large\n")); err == nil {
		t.Fatal("Swift clipboard image error was ignored")
	}
}

func TestClipboardImageDecodedBudgetAndPrivateTempFile(t *testing.T) {
	if err := validateClipboardImageConfig(image.Config{Width: 4096, Height: 4096}); err != nil {
		t.Fatalf("image at decoded-byte budget rejected: %v", err)
	}
	if err := validateClipboardImageConfig(image.Config{Width: 5000, Height: 5000}); err == nil || !strings.Contains(err.Error(), "decoded data") {
		t.Fatalf("image over decoded-byte budget error = %v", err)
	}
	if err := validateClipboardImageConfig(image.Config{Width: 5001, Height: 5000}); err == nil || !strings.Contains(err.Error(), "pixel limit") {
		t.Fatalf("image over pixel budget error = %v", err)
	}

	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	tempDir := privateClipboardTestDir(t)
	path, err := saveImageBytes(encoded.Bytes(), tempDir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("clipboard image mode = %04o, want 0600", info.Mode().Perm())
	}
}

func TestPasteUsesCurrentBracketedPasteMode(t *testing.T) {
	for _, test := range []struct {
		name        string
		initialMode bool
		currentMode bool
		want        string
	}{
		{name: "disabled-during-read", initialMode: true, currentMode: false, want: "paste"},
		{name: "enabled-during-read", initialMode: false, currentMode: true, want: "\x1b[200~paste\x1b[201~"},
	} {
		t.Run(test.name, func(t *testing.T) {
			pty, err := Spawn("/bin/sh", []string{"-c", "stty -echo -icanon min 1 time 0; printf READY; exec cat"})
			if err != nil {
				t.Fatal(err)
			}
			defer pty.Close()
			pty.setWriteGeneration(1)
			startup := make(chan any, 1)
			go func() { startup <- pty.Listen("paste-session")() }()
			select {
			case msg := <-startup:
				if ready, ok := msg.(PtyOutputMsg); !ok || string(ready.Data) != "READY" {
					t.Fatalf("PTY setup handshake = %#v, want READY", msg)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for PTY setup handshake")
			}

			emulator := NewEmulator("paste-session", "Paste", "/bin/sh", nil)
			emulator.pty = pty
			defer emulator.Close()
			emulator.screen = NewScreen(24, 80)
			emulator.listenerGeneration = 1
			emulator.screen.bracketedPaste = test.initialMode

			readStarted := make(chan struct{})
			continueRead := make(chan struct{})
			emulator.readClipboard = func(string) (string, string, error) {
				close(readStarted)
				<-continueRead
				return "paste", "", nil
			}
			command := emulator.PasteFromClipboard()
			commandDone := make(chan any, 1)
			go func() { commandDone <- command() }()
			<-readStarted

			emulator.mu.Lock()
			emulator.screen.bracketedPaste = test.currentMode
			emulator.mu.Unlock()
			close(continueRead)
			if msg := <-commandDone; msg != nil {
				t.Fatalf("paste command returned unexpected message: %#v", msg)
			}

			var output strings.Builder
			deadline := time.Now().Add(2 * time.Second)
			for output.Len() < len(test.want) {
				message := make(chan any, 1)
				go func() { message <- pty.Listen("paste-session")() }()
				select {
				case msg := <-message:
					switch msg := msg.(type) {
					case PtyOutputMsg:
						output.Write(msg.Data)
					case PtyExitMsg:
						t.Fatalf("PTY exited before paste output: %v", msg.Err)
					default:
						t.Fatalf("unexpected PTY message %T", msg)
					}
				case <-time.After(time.Until(deadline)):
					t.Fatalf("paste output = %q, want %q", output.String(), test.want)
				}
			}
			if got := output.String(); got != test.want {
				t.Fatalf("paste output = %q, want %q", got, test.want)
			}
		})
	}
}

func fakeWlPaste(t *testing.T, mimeTypes, textPayload, imagePayload []byte) string {
	t.Helper()
	binDir := t.TempDir()
	fixtureDir := t.TempDir()
	writeFixture := func(name string, data []byte) string {
		path := filepath.Join(fixtureDir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	t.Setenv("WL_TYPES_FILE", writeFixture("types", mimeTypes))
	t.Setenv("WL_TEXT_FILE", writeFixture("text", textPayload))
	t.Setenv("WL_IMAGE_FILE", writeFixture("image", imagePayload))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := `#!/bin/sh
if [ "$1" = "--list-types" ]; then
    cat "$WL_TYPES_FILE"
    exit 0
fi
mime=""
while [ "$#" -gt 0 ]; do
    if [ "$1" = "--type" ]; then
        shift
        mime="$1"
    fi
    shift
done
case "$mime" in
    image/png) cat "$WL_IMAGE_FILE" ;;
    text/plain\;charset=utf-8|text/plain|UTF8_STRING|STRING) cat "$WL_TEXT_FILE" ;;
    *) exit 3 ;;
esac
`
	toolPath := filepath.Join(binDir, "wl-paste")
	if err := os.WriteFile(toolPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return toolPath
}

func TestPasteWaylandUsesAdvertisedMIMEOnly(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}

	t.Run("text MIME", func(t *testing.T) {
		fakeWlPaste(t, []byte("text/plain;charset=utf-8\n"), []byte("hello"), nil)
		text, imagePath, err := pasteWayland(t.TempDir())
		if err != nil || text != "hello" || imagePath != "" {
			t.Fatalf("pasteWayland text = %q, %q, %v", text, imagePath, err)
		}
	})

	t.Run("PNG MIME", func(t *testing.T) {
		fakeWlPaste(t, []byte("image/png\n"), nil, encoded.Bytes())
		tempDir := privateClipboardTestDir(t)
		text, imagePath, err := pasteWayland(tempDir)
		if err != nil || text != "" || imagePath == "" {
			t.Fatalf("pasteWayland image = %q, %q, %v", text, imagePath, err)
		}
		if filepath.Dir(imagePath) != tempDir {
			t.Fatalf("image path %q escaped temp dir %q", imagePath, tempDir)
		}
		info, err := os.Stat(imagePath)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("private image file = %v, %v", info, err)
		}
	})

	t.Run("unsupported binary MIME is not guessed as PNG", func(t *testing.T) {
		fakeWlPaste(t, []byte("application/octet-stream\n"), encoded.Bytes(), encoded.Bytes())
		if _, _, err := pasteWayland(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no supported") {
			t.Fatalf("unsupported MIME error = %v", err)
		}
	})
}

func TestClipboardTempStorePermissionsQuotaExpiryAndCleanup(t *testing.T) {
	policy := ClipboardTempPolicy{MaxFiles: 1, MaxBytes: 1 << 20, TTL: time.Hour}
	store, err := newClipboardTempStore(policy)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	info, err := os.Stat(store.directory())
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("private directory = %v, %v", info, err)
	}

	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	first, err := saveImageBytes(encoded.Bytes(), store.directory())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.register(first); err != nil {
		t.Fatal(err)
	}
	second, err := saveImageBytes(encoded.Bytes(), store.directory())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.register(second); err == nil {
		t.Fatal("per-session file quota was not enforced")
	}
	if err := store.discard(second); err != nil {
		t.Fatal(err)
	}
	if err := store.expire(first); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("expired image remains: %v", err)
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.directory()); !os.IsNotExist(err) {
		t.Fatalf("private directory remains after close: %v", err)
	}
}
