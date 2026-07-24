package main

import (
	"os"
	"sync"
)

// cappedFile is an append writer for the log file that keeps it under a byte cap
// by dropping the OLDEST lines when the cap is crossed. Trimming happens only when
// crossing the cap (then down to ~half), so the occasional read+rewrite is cheap
// and the file oscillates roughly between max/2 and max.
type cappedFile struct {
	mu   sync.Mutex
	path string
	file *os.File
	size int64
	max  int64 // 0 = unlimited
	low  int64 // trim target
}

// newCappedFile opens path for appending, capped at max bytes (0 = unlimited).
// A file that is already over the cap is trimmed straight away.
func newCappedFile(path string, max int64) (*cappedFile, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	var size int64
	if info, err := file.Stat(); err == nil {
		size = info.Size()
	}
	cf := &cappedFile{path: path, file: file, size: size, max: max, low: max / 2}
	if max > 0 && size > max {
		cf.trim()
	}
	return cf, nil
}

// Write appends to the log, trimming it once the write pushes it over the cap.
func (cf *cappedFile) Write(p []byte) (int, error) {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	n, err := cf.file.Write(p)
	cf.size += int64(n)
	if cf.max > 0 && cf.size > cf.max {
		cf.trim()
	}
	return n, err
}

// trim keeps the last ~low bytes, cut at a line boundary. Called under cf.mu.
func (cf *cappedFile) trim() {
	cf.file.Close()
	data, err := os.ReadFile(cf.path)
	if err != nil {
		cf.reopen()
		return
	}
	keep := cf.low
	if keep <= 0 || keep > int64(len(data)) {
		keep = int64(len(data))
	}
	cut := int64(len(data)) - keep
	for cut < int64(len(data)) && data[cut] != '\n' { // align to next line start
		cut++
	}
	if cut < int64(len(data)) && data[cut] == '\n' {
		cut++
	}
	tail := data[cut:]
	if err := atomicWrite(cf.path, tail); err != nil {
		cf.reopen()
		return
	}
	cf.reopen()
	cf.size = int64(len(tail))
}

// reopen re-establishes the append handle after trim or Truncate replaced the
// file underneath it. Called under cf.mu.
func (cf *cappedFile) reopen() {
	if file, err := os.OpenFile(cf.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		cf.file = file
	}
}

// Truncate empties the log file and resets the size counter, keeping the writer
// usable. Used by the clear-logs command.
func (cf *cappedFile) Truncate() error {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	cf.file.Close()
	if err := os.Truncate(cf.path, 0); err != nil && !os.IsNotExist(err) {
		cf.reopen()
		return err
	}
	cf.reopen()
	cf.size = 0
	return nil
}
