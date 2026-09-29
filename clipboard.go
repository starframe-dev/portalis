package portalis

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	maxClipboardBytes       = 100 << 20
	maxClipboardCells       = 4 << 20
	maxClipboardImagePixels = 25_000_000
	clipboardCommandTimeout = 5 * time.Second
)

var errClipboardSwiftUnavailable = errors.New("swift clipboard reader unavailable")

func commandOutputLimited(stdin, name string, args ...string) ([]byte, error) {
	return commandOutputLimitedWithLimit(stdin, maxClipboardBytes, clipboardCommandTimeout, name, args...)
}

func commandOutputLimitedWithLimit(stdin string, maxBytes int, timeout time.Duration, name string, args ...string) ([]byte, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("clipboard output limit must be non-negative")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(stdout, int64(maxBytes)+1))
	if len(data) > maxBytes {
		cancel()
		_ = cmd.Wait()
		return nil, fmt.Errorf("%s output exceeds %d bytes", name, maxBytes)
	}
	if readErr != nil {
		cancel()
		_ = cmd.Wait()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s timed out", name)
		}
		return nil, readErr
	}
	waitErr := cmd.Wait()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%s timed out", name)
	}
	if waitErr != nil {
		return data, waitErr
	}
	return data, nil
}

// copyToClipboard sends the given lines (newline-joined) to the system clipboard.
func joinClipboardLines(lines []string, maxBytes int) (string, error) {
	if maxBytes < 0 {
		return "", fmt.Errorf("clipboard text limit must be non-negative")
	}
	total := 0
	for i, line := range lines {
		if i > 0 {
			if total == maxBytes {
				return "", fmt.Errorf("clipboard text exceeds %d bytes", maxBytes)
			}
			total++
		}
		if len(line) > maxBytes-total {
			return "", fmt.Errorf("clipboard text exceeds %d bytes", maxBytes)
		}
		total += len(line)
	}
	var text strings.Builder
	text.Grow(total)
	for i, line := range lines {
		if i > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(line)
	}
	return text.String(), nil
}

func copyToClipboard(lines []string) error {
	text, err := joinClipboardLines(lines, maxClipboardBytes)
	if err != nil {
		return err
	}
	if text == "" {
		return nil
	}
	// Try macOS pbcopy first, then Linux xclip / xsel / wl-copy.
	candidates := [][]string{
		{"pbcopy"},
		{"xclip", "-selection", "clipboard"},
		{"xsel", "--clipboard", "--input"},
		{"wl-copy"},
	}
	var lastErr error
	for _, cmd := range candidates {
		if _, err := exec.LookPath(cmd[0]); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), clipboardCommandTimeout)
			c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
			c.Stdin = strings.NewReader(text)
			err := c.Run()
			cancel()
			if err == nil {
				return nil
			} else {
				lastErr = err
			}
		}
	}
	if lastErr != nil {
		return fmt.Errorf("copy clipboard: %w", lastErr)
	}
	return fmt.Errorf("no clipboard tool available")
}

// pasteFromClipboard returns the clipboard contents. If the clipboard
// contains an image, it is saved to a temp file and the file path is
// returned as `imagePath`. Otherwise the plain-text contents are
// returned as `text`.
func pasteFromClipboard() (text string, imagePath string, err error) {
	if _, err := exec.LookPath("pbpaste"); err == nil {
		return pasteMac()
	}
	if _, err := exec.LookPath("wl-paste"); err == nil {
		return pasteWayland()
	}
	if _, err := exec.LookPath("xclip"); err == nil {
		return pasteX11()
	}
	return "", "", fmt.Errorf("no clipboard tool available")
}

// clipboardImageSwift is an inline Swift script that dumps the clipboard
// image (PNG/JPEG/PDF/TIFF) to a temp file and prints the path. macOS
// `pbpaste` only returns the text representation, so we need a Cocoa
// round-trip to access the `public.png` (or other image) UTI.
const clipboardImageSwift = `import Cocoa
import Foundation

let pb = NSPasteboard.general
let candidates: [(NSPasteboard.PasteboardType, String)] = [
    (NSPasteboard.PasteboardType("public.png"), "png"),
    (NSPasteboard.PasteboardType("public.jpeg"), "jpg"),
    (NSPasteboard.PasteboardType("public.tiff"), "tiff"),
    (NSPasteboard.PasteboardType("com.adobe.pdf"), "pdf"),
]
for (t, ext) in candidates {
    if let data = pb.data(forType: t) {
        if data.count > __MAX_CLIPBOARD_BYTES__ {
            print("ERR:clipboard image too large")
            exit(1)
        }
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("portalis-paste-\(UUID().uuidString).\(ext)")
        do {
            try data.write(to: url)
            print("PATH:\(url.path)")
        } catch {
            print("ERR:\(error)")
        }
        exit(0)
    }
}
print("NO_IMAGE")
`

func clipboardImageSwiftScript() string {
	return strings.Replace(clipboardImageSwift, "__MAX_CLIPBOARD_BYTES__", fmt.Sprint(maxClipboardBytes), 1)
}

// pasteMac reads the macOS clipboard. Order:
//  1. Try the inline Swift reader for any image UTI (works for any
//     source app — Preview, screenshots, browsers, etc.).
//  2. Fall back to pbpaste for text. pbpaste never returns raw image
//     bytes, so the inline Swift path is the only reliable image read.
func pasteMac() (string, string, error) {
	path, imageErr := pasteMacImage()
	if imageErr != nil && !errors.Is(imageErr, errClipboardSwiftUnavailable) {
		return "", "", imageErr
	}
	if path != "" {
		info, statErr := os.Stat(path)
		if statErr != nil {
			_ = os.Remove(path)
			return "", "", statErr
		}
		if info.Size() <= 0 {
			_ = os.Remove(path)
			return "", "", fmt.Errorf("clipboard image file is empty")
		}
		if info.Size() > maxClipboardBytes {
			_ = os.Remove(path)
			return "", "", fmt.Errorf("clipboard image exceeds %d bytes", maxClipboardBytes)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			_ = os.Remove(path)
			return "", "", err
		}
		return "", path, nil
	}
	out, err := commandOutputLimited("", "pbpaste")
	if err != nil {
		return "", "", err
	}
	// Defensive: if pbpaste ever does return raw bytes (e.g. external tool
	// pipes the image straight to pbcopy), still detect by PNG header.
	if len(out) >= 8 && bytes.HasPrefix(out, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) {
		path, err := saveImageBytes(out)
		if err == nil {
			return "", path, nil
		}
	}
	return string(out), "", nil
}

// pasteMacImage invokes the Swift clipboard reader via stdin and returns
// the saved file path (or "" if no image is on the clipboard).
func pasteMacImage() (string, error) {
	if _, err := exec.LookPath("swift"); err != nil {
		return "", fmt.Errorf("%w: %v", errClipboardSwiftUnavailable, err)
	}
	out, commandErr := commandOutputLimited(clipboardImageSwiftScript(), "swift", "-")
	line := strings.TrimSpace(string(out))
	if commandErr != nil && line == "" {
		return "", commandErr
	}
	path, parseErr := parseClipboardImageSwiftOutput(out)
	if parseErr != nil {
		return "", parseErr
	}
	if commandErr != nil {
		if path != "" {
			_ = os.Remove(path)
		}
		return "", commandErr
	}
	return path, nil
}

func parseClipboardImageSwiftOutput(out []byte) (string, error) {
	line := strings.TrimSpace(string(out))
	if line == "NO_IMAGE" {
		return "", nil
	}
	if strings.HasPrefix(line, "PATH:") {
		path := strings.TrimPrefix(line, "PATH:")
		if path != "" {
			return path, nil
		}
	}
	if strings.HasPrefix(line, "ERR:") {
		return "", fmt.Errorf("clipboard image: %s", strings.TrimPrefix(line, "ERR:"))
	}
	return "", fmt.Errorf("unexpected swift output: %q", line)
}

// pasteWayland uses wl-paste. Image transfer requires --type image/png.
func pasteWayland() (string, string, error) {
	// Try text first.
	if out, err := commandOutputLimited("", "wl-paste", "--no-newline"); err == nil {
		// Could be text or PNG bytes — check signature.
		if len(out) >= 8 && bytes.HasPrefix(out, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) {
			path, err := saveImageBytes(out)
			return "", path, err
		}
		return string(out), "", nil
	}
	// Try image.
	out, err := commandOutputLimited("", "wl-paste", "--type", "image/png")
	if err != nil {
		return "", "", err
	}
	path, err := saveImageBytes(out)
	return "", path, err
}

// pasteX11 uses xclip -selection clipboard -o.
func pasteX11() (string, string, error) {
	out, err := commandOutputLimited("", "xclip", "-selection", "clipboard", "-o", "-t", "image/png")
	if err == nil && len(out) > 0 {
		path, err := saveImageBytes(out)
		return "", path, err
	}
	out, err = commandOutputLimited("", "xclip", "-selection", "clipboard", "-o")
	if err != nil {
		return "", "", err
	}
	return string(out), "", nil
}

func validateClipboardImageConfig(config image.Config) error {
	width, height := int64(config.Width), int64(config.Height)
	if width <= 0 || height <= 0 || width > int64(maxClipboardImagePixels)/height {
		return fmt.Errorf("clipboard image dimensions %dx%d exceed limit", config.Width, config.Height)
	}
	return nil
}

func saveImageBytes(data []byte) (string, error) {
	if len(data) > maxClipboardBytes {
		return "", fmt.Errorf("clipboard image exceeds %d bytes", maxClipboardBytes)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	if err := validateClipboardImageConfig(config); err != nil {
		return "", err
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(os.TempDir(), "portalis-paste-*.png")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := png.Encode(file, img); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// ClipboardErrorMsg reports a system clipboard failure without terminating PTY.
type ClipboardErrorMsg struct {
	Err error
}

// PasteFromClipboard asynchronously reads the system clipboard and writes its
// text (or an image temp-file path) to the current PTY.
func (e *Emulator) PasteFromClipboard() tea.Cmd {
	e.mu.RLock()
	pty := e.pty
	generation := e.listenerGeneration
	readClipboard := e.readClipboard
	e.mu.RUnlock()
	if pty == nil {
		return nil
	}
	if readClipboard == nil {
		readClipboard = pasteFromClipboard
	}

	return func() tea.Msg {
		text, imagePath, err := readClipboard()
		if err != nil {
			return ClipboardErrorMsg{Err: err}
		}
		if text == "" && imagePath == "" {
			return nil
		}

		e.mu.RLock()
		current := e.pty == pty && e.listenerGeneration == generation
		bracketed := current && e.screen != nil && e.screen.bracketedPaste
		e.mu.RUnlock()
		if !current {
			if imagePath != "" {
				_ = os.Remove(imagePath)
			}
			return nil
		}

		var payload []byte
		if imagePath != "" {
			payload = []byte(imagePath + "\n")
		} else if bracketed {
			payload = append([]byte("\x1b[200~"), []byte(text)...)
			payload = append(payload, []byte("\x1b[201~")...)
		} else {
			payload = []byte(text)
		}
		if err := pty.WriteForGeneration(generation, payload); err != nil {
			if imagePath != "" {
				_ = os.Remove(imagePath)
			}
			return PtyExitMsg{SessionID: e.SessionID, Generation: generation, Err: err}
		}
		if imagePath != "" {
			e.mu.Lock()
			if e.pty == pty && e.listenerGeneration == generation {
				e.tempFiles = append(e.tempFiles, imagePath)
			} else {
				_ = os.Remove(imagePath)
			}
			e.mu.Unlock()
		}
		return nil
	}
}
