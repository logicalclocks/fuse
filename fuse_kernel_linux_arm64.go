package fuse

// O_LARGEFILE in the arm64 kernel ABI. syscall and x/sys/unix define it as
// 0 on 64-bit platforms, and 0x8000 is O_NOFOLLOW here.
const kernelOLargefile = 0x20000
