package fuse

import (
	"runtime"
	"syscall"
	"testing"
)

// A 64-bit kernel adds O_LARGEFILE to every open, at its architecture's
// value; syscall defines it as 0 on 64-bit platforms, so the values are
// spelled out here.
var kernelLargefileForTest = map[string]uint32{
	"386":   0x8000,
	"amd64": 0x8000,
	"arm64": 0x20000,
}

func TestOpenFlagsStripsOnlyLargefile(t *testing.T) {
	largefile, ok := kernelLargefileForTest[runtime.GOARCH]
	if !ok {
		t.Skipf("no O_LARGEFILE value for %s", runtime.GOARCH)
	}
	kept := uint32(syscall.O_RDWR | syscall.O_APPEND | syscall.O_NOFOLLOW)
	if g, e := openFlags(kept|largefile), OpenFlags(kept); g != e {
		t.Fatalf("openFlags(%#x) = %v, want %v", kept|largefile, g, e)
	}
}
