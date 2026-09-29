//go:build darwin

package portalis

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const clipboardIntegrationEnv = "PORTALIS_RUN_CLIPBOARD_INTEGRATION"

func requireClipboardIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv(clipboardIntegrationEnv) != "1" {
		t.Skipf("set %s=1 to replace the system clipboard and run this integration test", clipboardIntegrationEnv)
	}
}

// loadImageIntoClipboard loads a PNG file into the macOS clipboard
// using an inline Swift script. Used by the test below.
func loadImageIntoClipboard(t *testing.T, path string) {
	t.Helper()
	if _, err := exec.LookPath("swift"); err != nil {
		t.Skip("swift not available")
	}
	const script = `import Cocoa
import Foundation

let url = URL(fileURLWithPath: CommandLine.arguments[1])
guard let data = try? Data(contentsOf: url) else {
    print("ERR")
    exit(1)
}
let pb = NSPasteboard.general
pb.clearContents()
_ = pb.setData(data, forType: NSPasteboard.PasteboardType("public.png"))
print("OK")
`
	tmp := filepath.Join(t.TempDir(), "loadclip_test.swift")
	if err := os.WriteFile(tmp, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp)
	if out, err := exec.Command("swift", tmp, path).CombinedOutput(); err != nil {
		t.Fatalf("loadclip: %v\n%s", err, out)
	}
}

func TestPasteMac_Image(t *testing.T) {
	requireClipboardIntegration(t)
	if _, err := exec.LookPath("swift"); err != nil {
		t.Skip("swift not available; skipping macOS image paste test")
	}
	png := writeSamplePNG(t)
	loadImageIntoClipboard(t, png)

	text, imgPath, err := pasteMac()
	if err != nil {
		t.Fatalf("pasteMac: %v", err)
	}
	if imgPath != "" {
		t.Cleanup(func() { _ = os.Remove(imgPath) })
	}
	if imgPath == "" {
		t.Fatalf("pasteMac returned no image path; text=%q", text)
	}
	if !strings.HasSuffix(imgPath, ".png") {
		t.Errorf("imgPath = %q, want .png suffix", imgPath)
	}
	// File must exist and have non-zero size.
	info, err := os.Stat(imgPath)
	if err != nil {
		t.Fatalf("stat %s: %v", imgPath, err)
	}
	if info.Size() == 0 {
		t.Error("image file is empty")
	}
	t.Logf("OK: text=%q imgPath=%s (%d bytes)", text, imgPath, info.Size())
}

func TestPasteMac_Text(t *testing.T) {
	requireClipboardIntegration(t)
	command := exec.Command("pbcopy")
	command.Stdin = strings.NewReader("hello world")
	if err := command.Run(); err != nil {
		t.Skipf("pbcopy unavailable: %v", err)
	}
	text, imgPath, err := pasteMac()
	if err != nil {
		t.Fatalf("pasteMac: %v", err)
	}
	if imgPath != "" {
		t.Errorf("unexpected image path: %q", imgPath)
	}
	if text != "hello world" {
		t.Errorf("text = %q, want %q", text, "hello world")
	}
}

func writeSamplePNG(t *testing.T) string {
	t.Helper()
	const pixel = `iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=`
	data, err := base64.StdEncoding.DecodeString(pixel)
	if err != nil {
		t.Fatalf("decode sample PNG: %v", err)
	}
	path := filepath.Join(t.TempDir(), "sample.png")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write sample PNG: %v", err)
	}
	return path
}
