package portalis

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

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
			c := exec.Command(cmd[0], cmd[1:]...)
			c.Stdin = strings.NewReader(text)
			if err := c.Run(); err == nil {
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
			const maxClipboardImageBytes = 100 << 20
			if info.Size() > maxClipboardImageBytes {
				_ = os.Remove(path)
				return "", "", fmt.Errorf("clipboard image exceeds %d bytes", maxClipboardImageBytes)
			}
			_ = os.Chmod(path, 0o600)
			return "", path, nil
		}
	}
	out, err := exec.Command("pbpaste").Output()
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
	cmd := exec.Command("swift", "-")
	cmd.Stdin = strings.NewReader(clipboardImageSwift)
	out, err := cmd.Output()
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
	if out, err := exec.Command("wl-paste", "--no-newline").Output(); err == nil {
		// Could be text or PNG bytes — check signature.
		if len(out) >= 8 && bytes.HasPrefix(out, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) {
			path, err := saveImageBytes(out)
			return "", path, err
		}
		return string(out), "", nil
	}
	// Try image.
	out, err := exec.Command("wl-paste", "--type", "image/png").Output()
	if err != nil {
		return "", "", err
	}
	path, err := saveImageBytes(out)
	return "", path, err
}

// pasteX11 uses xclip -selection clipboard -o.
func pasteX11() (string, string, error) {
	out, err := exec.Command("xclip", "-selection", "clipboard", "-o", "-t", "image/png").Output()
	if err == nil && len(out) > 0 {
		path, err := saveImageBytes(out)
		return "", path, err
	}
	out, err = exec.Command("xclip", "-selection", "clipboard", "-o").Output()
	if err != nil {
		return "", "", err
	}
	return string(out), "", nil
}

func saveImageBytes(data []byte) (string, error) {
	const maxClipboardImageBytes = 100 << 20
	const maxClipboardImagePixels = 100_000_000
	if len(data) > maxClipboardImageBytes {
		return "", fmt.Errorf("clipboard image exceeds %d bytes", maxClipboardImageBytes)
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
			return PtyExitMsg{SessionID: e.SessionID, Generation: generation, Err: err}
		}
		return nil
	}
}
