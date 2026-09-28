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
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	maxClipboardBytes       = 100 << 20
	maxClipboardImagePixels = 100_000_000
	clipboardCommandTimeout = 5 * time.Second
)

func commandOutputLimited(stdin string, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), clipboardCommandTimeout)
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
	data, readErr := io.ReadAll(io.LimitReader(stdout, int64(maxClipboardBytes)+1))
	if len(data) > maxClipboardBytes {
		cancel()
		_ = cmd.Wait()
		return nil, fmt.Errorf("%s output exceeds %d bytes", name, maxClipboardBytes)
	}
	if readErr != nil {
		cancel()
		_ = cmd.Wait()
		return nil, readErr
	}
	waitErr := cmd.Wait()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%s timed out", name)
	}
	if waitErr != nil {
		return nil, waitErr
	}
	return data, nil
}

// copyToClipboard sends the given lines (newline-joined) to the system clipboard.
func copyToClipboard(lines []string) error {
	text := strings.Join(lines, "\n")
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
        if data.count > 104857600 {
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

// pasteMac reads the macOS clipboard. Order:
//  1. Try the inline Swift reader for any image UTI (works for any
//     source app — Preview, screenshots, browsers, etc.).
//  2. Fall back to pbpaste for text. pbpaste never returns raw image
//     bytes, so the inline Swift path is the only reliable image read.
func pasteMac() (string, string, error) {
	if path, err := pasteMacImage(); err == nil && path != "" {
		// Confirm it actually exists and is non-empty.
		if info, statErr := os.Stat(path); statErr == nil && info.Size() > 0 {
			if info.Size() > maxClipboardBytes {
				_ = os.Remove(path)
				return "", "", fmt.Errorf("clipboard image exceeds %d bytes", maxClipboardBytes)
			}
			_ = os.Chmod(path, 0o600)
			return "", path, nil
		}
		_ = os.Remove(path)
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
		return "", err
	}
	out, err := commandOutputLimited(clipboardImageSwift, "swift", "-")
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(out))
	if line == "NO_IMAGE" {
		return "", nil
	}
	if strings.HasPrefix(line, "PATH:") {
		return strings.TrimPrefix(line, "PATH:"), nil
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

func saveImageBytes(data []byte) (string, error) {
	if len(data) > maxClipboardBytes {
		return "", fmt.Errorf("clipboard image exceeds %d bytes", maxClipboardBytes)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > maxClipboardImagePixels {
		return "", fmt.Errorf("clipboard image dimensions %dx%d exceed limit", config.Width, config.Height)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	dir := filepath.Join(os.TempDir(), "portalis-clip")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(dir, "paste-*.png")
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
	bracketed := e.screen != nil && e.screen.bracketedPaste
	e.mu.RUnlock()
	if pty == nil {
		return nil
	}

	return func() tea.Msg {
		text, imagePath, err := pasteFromClipboard()
		if err != nil {
			return ClipboardErrorMsg{Err: err}
		}
		if text == "" && imagePath == "" {
			return nil
		}

		e.mu.RLock()
		current := e.pty == pty && e.listenerGeneration == generation
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
		if err := pty.Write(payload); err != nil {
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
