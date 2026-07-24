package main

import (
	"strings"
	"testing"
)

func TestParseEipsFlag(t *testing.T) {
	rest, eipsOut := parseEipsFlag([]string{"-y", "-eips", "name"})
	if !eipsOut {
		t.Fatal("-eips not detected")
	}
	if strings.Join(rest, ",") != "-y,name" {
		t.Fatalf("rest = %v, want [-y name]", rest)
	}
	if _, on := parseEipsFlag([]string{"-y"}); on {
		t.Fatal("-eips reported present when absent")
	}
}
