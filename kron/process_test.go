package main

import "testing"

func TestParseRootReadonly(t *testing.T) {
	const roMounts = "rootfs / rootfs rw 0 0\n/dev/mmcblk0p1 / ext4 ro,relatime 0 0\n" +
		"proc /proc proc rw,nosuid 0 0\n"
	const rwMounts = "/dev/mmcblk0p1 / ext4 rw,relatime 0 0\n"

	if ro, ok := parseRootReadonly(roMounts); !ok || !ro {
		t.Fatalf("read-only root: got ro=%v ok=%v, want true true", ro, ok)
	}
	if ro, ok := parseRootReadonly(rwMounts); !ok || ro {
		t.Fatalf("writable root: got ro=%v ok=%v, want false true", ro, ok)
	}
	if _, ok := parseRootReadonly("proc /proc proc rw 0 0\n"); ok {
		t.Fatal("no root entry: ok=true, want false")
	}
}
