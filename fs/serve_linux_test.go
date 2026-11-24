package fs_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"

	"bazil.org/fuse"
	"bazil.org/fuse/fs/fstestutil"
	"bazil.org/fuse/fs/fstestutil/spawntest/httpjson"
	"golang.org/x/sys/unix"
)

func platformStatfs(st *syscall.Statfs_t) *statfsResult {
	return &statfsResult{
		Blocks:  st.Blocks,
		Bfree:   st.Bfree,
		Bavail:  st.Bavail,
		Files:   st.Files,
		Ffree:   st.Ffree,
		Bsize:   st.Bsize,
		Namelen: st.Namelen,
		Frsize:  st.Frsize,
	}
}

func platformStat(fi os.FileInfo) *statResult {
	r := &statResult{
		Mode: fi.Mode(),
	}
	st := fi.Sys().(*syscall.Stat_t)
	r.Ino = st.Ino
	r.Nlink = uint64(st.Nlink)
	r.UID = st.Uid
	r.GID = st.Gid
	r.Blksize = int64(st.Blksize)
	return r
}

var _lockOFDHelper = helpers.Register("lock-ofd", &lockHelp{
	lockFn: func(fd uintptr, req *lockReq) error {
		lk := unix.Flock_t{
			Type:   unix.F_WRLCK,
			Whence: int16(io.SeekStart),
			Start:  req.Start,
			Len:    req.Len,
		}
		cmd := unix.F_OFD_SETLK
		if req.Wait {
			cmd = unix.F_OFD_SETLKW
		}
		return unix.FcntlFlock(fd, cmd, &lk)
	},
	unlockFn: func(fd uintptr, req *lockReq) error {
		lk := unix.Flock_t{
			Type:   unix.F_UNLCK,
			Whence: int16(io.SeekStart),
			Start:  req.Start,
			Len:    req.Len,
		}
		cmd := unix.F_OFD_SETLK
		if req.Wait {
			cmd = unix.F_OFD_SETLKW
		}
		return unix.FcntlFlock(fd, cmd, &lk)
	},
	queryFn: func(fd uintptr, lk *unix.Flock_t) error {
		cmd := unix.F_OFD_GETLK
		return unix.FcntlFlock(fd, cmd, lk)
	},
})

func init() {
	lockOFDHelper = _lockOFDHelper
}

// Test readdir offsets through the kernel: a directory read in getdents
// calls too small for one reply, seeks to an offset getdents returned,
// past the end and back to the start.

type readDirMany struct {
	fstestutil.Dir
	entries []fuse.Dirent
}

func (d *readDirMany) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	return d.entries, nil
}

type readdirOffsetsRequest struct {
	Path      string
	SeekIndex int
}

type readdirOffsetsResult struct {
	Names       []string
	AfterSeek   []string
	PastEnd     int
	AfterRewind []string
}

// getdentsNames reads fd to the end in getdents calls of bufSize bytes and
// returns each name with the offset getdents gave it.
func getdentsNames(fd int, bufSize int) ([]string, []int64, error) {
	var names []string
	var offs []int64
	buf := make([]byte, bufSize)
	for {
		n, err := syscall.Getdents(fd, buf)
		if err != nil {
			return nil, nil, err
		}
		if n == 0 {
			return names, offs, nil
		}
		for b := buf[:n]; len(b) > 0; {
			reclen := int(binary.LittleEndian.Uint16(b[16:18]))
			name := b[19:reclen]
			if i := bytes.IndexByte(name, 0); i >= 0 {
				name = name[:i]
			}
			names = append(names, string(name))
			offs = append(offs, int64(binary.LittleEndian.Uint64(b[8:16])))
			b = b[reclen:]
		}
	}
}

func doReaddirOffsets(ctx context.Context, req readdirOffsetsRequest) (*readdirOffsetsResult, error) {
	fd, err := syscall.Open(req.Path, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	var res readdirOffsetsResult
	var offs []int64
	if res.Names, offs, err = getdentsNames(fd, 512); err != nil {
		return nil, err
	}
	if req.SeekIndex >= len(offs) {
		return nil, fmt.Errorf("listed %d entries, cannot seek after entry %d", len(offs), req.SeekIndex)
	}
	if _, err := syscall.Seek(fd, offs[req.SeekIndex], io.SeekStart); err != nil {
		return nil, err
	}
	if res.AfterSeek, _, err = getdentsNames(fd, 512); err != nil {
		return nil, err
	}
	if _, err := syscall.Seek(fd, 1<<40, io.SeekStart); err != nil {
		return nil, err
	}
	if res.PastEnd, err = syscall.Getdents(fd, make([]byte, 512)); err != nil {
		return nil, err
	}
	if _, err := syscall.Seek(fd, 0, io.SeekStart); err != nil {
		return nil, err
	}
	if res.AfterRewind, _, err = getdentsNames(fd, 512); err != nil {
		return nil, err
	}
	return &res, nil
}

var readdirOffsetsHelper = helpers.Register("readdirOffsets", httpjson.ServePOST(doReaddirOffsets))

func TestReadDirOffsets(t *testing.T) {
	maybeParallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &readDirMany{}
	var want []string
	for i := 0; i < 500; i++ {
		name := fmt.Sprintf("entry-%04d-%s", i, strings.Repeat("x", 100))
		f.entries = append(f.entries, fuse.Dirent{Name: name, Inode: uint64(1000 + i), Type: fuse.DT_File})
		want = append(want, name)
	}
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{Node: f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := readdirOffsetsHelper.Spawn(ctx, t)
	defer control.Close()

	const seekIndex = 199
	var res readdirOffsetsResult
	if err := control.JSON("/").Call(ctx, readdirOffsetsRequest{Path: mnt.Dir, SeekIndex: seekIndex}, &res); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	checkNames := func(what string, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %d names, want %d", what, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: name %d is %q, want %q", what, i, got[i], want[i])
			}
		}
	}
	checkNames("whole directory", res.Names, want)
	checkNames("after seeking to an offset getdents returned", res.AfterSeek, want[seekIndex+1:])
	if res.PastEnd != 0 {
		t.Errorf("getdents after seeking past the end returned %d bytes, want 0", res.PastEnd)
	}
	checkNames("after rewinding", res.AfterRewind, want)
}
