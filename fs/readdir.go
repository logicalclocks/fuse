package fs

import (
	"syscall"

	"bazil.org/fuse"
)

// appendDirents encodes dirents from index offset into buf, whole records
// only and at most size bytes. Each record's offset is the index of the
// next entry, so a later request at any offset this returned continues
// after that entry and one at 0 starts again; an offset outside the
// list is the end of the directory.
func appendDirents(buf []byte, dirents []fuse.Dirent, offset int64, size int) ([]byte, error) {
	if offset < 0 || offset >= int64(len(dirents)) {
		return buf, nil
	}
	start := len(buf)
	for i := offset; i < int64(len(dirents)); i++ {
		end := len(buf)
		buf = fuse.AppendDirentAt(buf, dirents[i], uint64(i+1))
		if len(buf)-start > size {
			if end == start {
				// The kernel asks for at least a page, too small only
				// for a name over 4072 bytes, which kernels from 6.15
				// accept.
				return buf[:start], syscall.EINVAL
			}
			return buf[:end], nil
		}
	}
	return buf, nil
}
