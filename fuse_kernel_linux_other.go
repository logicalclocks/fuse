//go:build linux && !arm64

package fuse

// O_LARGEFILE in the asm-generic kernel ABI, which amd64 and 386 use.
// The other architectures with a value of their own (arm, powerpc, mips,
// sparc, parisc) still get this one, as before arm64 got its own.
const kernelOLargefile = 0x8000
