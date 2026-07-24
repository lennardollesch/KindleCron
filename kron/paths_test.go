package main

import (
	"bytes"
	"io"
	"testing"
)

// alwaysErrWriter fails every write, standing in for a stdout/stderr that KUAL
// has closed (EPIPE).
type alwaysErrWriter struct{}

func (alwaysErrWriter) Write(p []byte) (int, error) { return 0, io.ErrClosedPipe }

func TestBestEffortSwallowsWriteError(t *testing.T) {
	n, err := bestEffort{alwaysErrWriter{}}.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("bestEffort.Write = (%d, %v), want (5, nil)", n, err)
	}

	// The KUAL case: wrapped in a MultiWriter, a failing (broken stderr) writer
	// must not stop a later writer (the log file) from receiving the line.
	var logSink bytes.Buffer
	writer := io.MultiWriter(bestEffort{alwaysErrWriter{}}, &logSink)
	if _, err := writer.Write([]byte("world")); err != nil {
		t.Fatalf("MultiWriter.Write err = %v, want nil", err)
	}
	if logSink.String() != "world" {
		t.Fatalf("log sink got %q, want %q", logSink.String(), "world")
	}
}
