package portalis

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const (
	defaultRawTraceMaxBytes int64 = 16 << 20
	defaultRawTraceMaxFiles       = 3
	maxRawTraceMaxBytes     int64 = 1 << 30
	maxRawTraceMaxFiles           = 16
)

type rawTraceConfig struct {
	maxBytes int64
	maxFiles int
}

func rawTraceConfigFromEnv() (rawTraceConfig, []error) {
	config := rawTraceConfig{maxBytes: defaultRawTraceMaxBytes, maxFiles: defaultRawTraceMaxFiles}
	var warnings []error
	if raw := strings.TrimSpace(os.Getenv("PORTALIS_RAW_TRACE_MAX_BYTES")); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 1 || value > maxRawTraceMaxBytes {
			warnings = append(warnings, fmt.Errorf("invalid PORTALIS_RAW_TRACE_MAX_BYTES %q; using default %d", raw, defaultRawTraceMaxBytes))
		} else {
			config.maxBytes = value
		}
	}
	if raw := strings.TrimSpace(os.Getenv("PORTALIS_RAW_TRACE_MAX_FILES")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > maxRawTraceMaxFiles {
			warnings = append(warnings, fmt.Errorf("invalid PORTALIS_RAW_TRACE_MAX_FILES %q; using default %d", raw, defaultRawTraceMaxFiles))
		} else {
			config.maxFiles = value
		}
	}
	return config, warnings
}

type rotatingTrace struct {
	basePath string
	maxBytes int64
	maxFiles int
	file     *os.File
	written  int64
}

func openRotatingTrace(path string, config rawTraceConfig) (*rotatingTrace, error) {
	trace := &rotatingTrace{basePath: path, maxBytes: config.maxBytes, maxFiles: config.maxFiles}
	if trace.maxBytes < 1 || trace.maxBytes > maxRawTraceMaxBytes || trace.maxFiles < 1 || trace.maxFiles > maxRawTraceMaxFiles {
		return nil, errors.New("invalid raw trace limits")
	}
	if err := trace.openNext(); err != nil {
		return nil, err
	}
	return trace, nil
}

func (t *rotatingTrace) openNext() error {
	file, err := os.OpenFile(t.basePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(t.basePath)
		return err
	}
	t.file = file
	t.written = 0
	return nil
}

func (t *rotatingTrace) Write(data []byte) (int, error) {
	if t.file == nil {
		return 0, os.ErrClosed
	}
	total := 0
	for len(data) > 0 {
		if t.written >= t.maxBytes {
			if err := t.rotate(); err != nil {
				return total, err
			}
		}
		remaining := t.maxBytes - t.written
		chunkSize := len(data)
		if int64(chunkSize) > remaining {
			chunkSize = int(remaining)
		}
		written, err := t.file.Write(data[:chunkSize])
		total += written
		t.written += int64(written)
		data = data[written:]
		if err != nil {
			return total, err
		}
		if written != chunkSize {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func (t *rotatingTrace) rotate() error {
	if err := t.file.Close(); err != nil {
		t.file = nil
		return err
	}
	t.file = nil
	if err := t.rotateFiles(); err != nil {
		return err
	}
	return t.openNext()
}

func (t *rotatingTrace) rotateFiles() error {
	for index := t.maxFiles - 1; index >= 1; index-- {
		source := t.basePath
		if index > 1 {
			source += "." + strconv.Itoa(index-1)
		}
		destination := t.basePath + "." + strconv.Itoa(index)
		if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(source, destination); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if t.maxFiles == 1 {
		if err := os.Remove(t.basePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (t *rotatingTrace) Close() error {
	if t.file == nil {
		return nil
	}
	file := t.file
	t.file = nil
	return file.Close()
}
