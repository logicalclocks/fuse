package fs

import (
	"context"
	"log"
	"strings"
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

// The kernel stops parsing a READDIRPLUS reply at a name that is
// empty, longer than its FUSE_NAME_MAX or holds '/', and never links
// or forgets that record or the ones after it. FUSE_NAME_MAX is 1024 up
// to Linux 6.14 and 4095 from 6.15; the library cannot tell which
// kernel it serves, so it keeps 1024, where a READDIR reply fails too
// before 6.15. A name holding NUL does not stop the parse, but no
// program can use it.
const maxDirentPlusName = 1024

func validDirentPlusName(name string) bool {
	return name != "" && len(name) <= maxDirentPlusName && !strings.ContainsAny(name, "/\x00")
}

// appendDirentsPlus is appendDirents for READDIRPLUS. The record of an
// entry lookup answers takes one reference on its node, which the
// kernel returns with a FORGET. "." and ".." go without attributes:
// the kernel never links them, so it would never return a reference
// taken for them. lookup may be nil, and then every entry is sent
// without attributes.
func (c *Server) appendDirentsPlus(ctx context.Context, buf []byte, dirents []fuse.Dirent, offset int64, size int, proto fuse.Protocol, parent *serveNode, lookup HandleReadDirPlusAller) ([]byte, error) {
	if offset < 0 || offset >= int64(len(dirents)) {
		return buf, nil
	}
	start := len(buf)
	var refs []fuse.NodeID
	for i := offset; i < int64(len(dirents)); i++ {
		dir := dirents[i]
		if !validDirentPlusName(dir.Name) {
			for _, id := range refs {
				c.forgetNode(id, 1)
			}
			log.Printf("fuse: readdirplus: invalid name %q in directory %v, failing the request", dir.Name, parent.node)
			return buf[:start], syscall.EIO
		}
		if len(buf)-start+fuse.DirentPlusSize(dir, proto) > size {
			if len(buf) == start {
				// The kernel asks for at least a page, which holds any
				// record of a name within maxDirentPlusName.
				return buf, syscall.EINVAL
			}
			break
		}
		var entry fuse.LookupResponse
		if lookup != nil && dir.Name != "." && dir.Name != ".." {
			initLookupResponse(&entry)
			entry.Attr = fuse.Attr{Valid: attrValidTime, Nlink: 1, Atime: startTime, Mtime: startTime, Ctime: startTime}
			if n, ok := lookup.LookupPlus(ctx, dir.Name, &entry.Attr); ok {
				if entry.Attr.Inode == 0 {
					entry.Attr.Inode = c.dynamicInode(parent.inode, dir.Name)
				}
				entry.Node, entry.Generation = c.saveNode(entry.Attr.Inode, n)
				refs = append(refs, entry.Node)
			}
		}
		buf = fuse.AppendDirentPlus(buf, &entry, dir, uint64(i+1), proto)
	}
	return buf, nil
}
