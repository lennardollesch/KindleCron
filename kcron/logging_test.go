package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCappedFileTrimsOldestLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kcron.log")
	capped, err := newCappedFile(path, 200) // trim target (low) = 100
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		fmt.Fprintf(capped, "line %02d\n", i)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) > 200 {
		t.Fatalf("file is %d bytes, want <= cap 200", len(data))
	}
	if strings.Contains(string(data), "line 00\n") {
		t.Fatal("oldest line should have been dropped")
	}
	if !strings.Contains(string(data), "line 59\n") {
		t.Fatal("newest line must be retained")
	}
}

func TestCappedFileUnlimited(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kcron.log")
	capped, err := newCappedFile(path, 0) // 0 = unlimited
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		fmt.Fprint(capped, "x\n")
	}
	data, _ := os.ReadFile(path)
	if len(data) != 200 {
		t.Fatalf("unlimited file = %d bytes, want 200 (no trimming)", len(data))
	}
}

func TestCappedFileTruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kcron.log")
	capped, err := newCappedFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(capped, "some content\n")
	if err := capped.Truncate(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if len(data) != 0 {
		t.Fatalf("after Truncate file = %d bytes, want 0", len(data))
	}
}
