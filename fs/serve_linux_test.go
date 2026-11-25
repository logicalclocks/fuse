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
	"time"
	"unsafe"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"bazil.org/fuse/fs/fstestutil"
	"bazil.org/fuse/fs/fstestutil/record"
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

// The encoders and the kernel write dirents in the host's byte order.
var hostEndian binary.ByteOrder = binary.LittleEndian

func init() {
	one := uint16(1)
	if *(*byte)(unsafe.Pointer(&one)) == 0 {
		hostEndian = binary.BigEndian
	}
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
			reclen := int(hostEndian.Uint16(b[16:18]))
			name := b[19:reclen]
			if i := bytes.IndexByte(name, 0); i >= 0 {
				name = name[:i]
			}
			names = append(names, string(name))
			offs = append(offs, int64(hostEndian.Uint64(b[8:16])))
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

// Test READDIRPLUS: a stat after a readdir is answered from the listing,
// without a LOOKUP, and every node the listing linked is forgotten once
// the kernel drops it.

type readDirPlusChild struct {
	size    uint64
	forgets *record.Counter
}

func (c *readDirPlusChild) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0o644
	a.Size = c.size
	return nil
}

func (c *readDirPlusChild) Forget() { c.forgets.Inc() }

// readDirPlusDir lists its children and counts lookups; it does not
// implement LookupPlus.
type readDirPlusDir struct {
	fstestutil.Dir
	names    []string
	children map[string]*readDirPlusChild
	lookups  record.Counter
	forgets  record.Counter
}

func newReadDirPlusDir(n int) *readDirPlusDir {
	d := &readDirPlusDir{children: map[string]*readDirPlusChild{}}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("child-%03d", i)
		d.names = append(d.names, name)
		d.children[name] = &readDirPlusChild{size: uint64(i + 1), forgets: &d.forgets}
	}
	return d
}

func (d *readDirPlusDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	var dirents []fuse.Dirent
	for i, name := range d.names {
		dirents = append(dirents, fuse.Dirent{Name: name, Inode: uint64(100 + i), Type: fuse.DT_File})
	}
	return dirents, nil
}

func (d *readDirPlusDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	d.lookups.Inc()
	if c, ok := d.children[name]; ok {
		return c, nil
	}
	return nil, syscall.ENOENT
}

type readDirPlusLookupDir struct {
	*readDirPlusDir
}

var _ fs.HandleReadDirPlusAller = readDirPlusLookupDir{}

func (d readDirPlusLookupDir) LookupPlus(ctx context.Context, name string, attr *fuse.Attr) (fs.Node, bool) {
	c, ok := d.children[name]
	if !ok {
		return nil, false
	}
	if err := c.Attr(ctx, attr); err != nil {
		return nil, false
	}
	return c, true
}

// doReaddirStat lists path and lstats every entry, as ls -l does.
func doReaddirStat(ctx context.Context, path string) (map[string]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sizes := map[string]int64{}
	for _, name := range names {
		fi, err := os.Lstat(path + "/" + name)
		if err != nil {
			return nil, err
		}
		sizes[name] = fi.Size()
	}
	return sizes, nil
}

var readdirStatHelper = helpers.Register("readdirStat", httpjson.ServePOST(doReaddirStat))

func readDirPlusStat(t *testing.T, root fs.Node, want *readDirPlusDir) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{Node: root}, nil, fuse.ReaddirPlus())
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	if mnt.Conn.Features()&fuse.InitDoReaddirplus == 0 {
		t.Fatalf("kernel did not take READDIRPLUS: features %v", mnt.Conn.Features())
	}
	control := readdirStatHelper.Spawn(ctx, t)
	defer control.Close()

	var sizes map[string]int64
	if err := control.JSON("/").Call(ctx, mnt.Dir, &sizes); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if len(sizes) != len(want.names) {
		t.Fatalf("listed %d entries, want %d: %v", len(sizes), len(want.names), sizes)
	}
	for _, name := range want.names {
		if got, want := sizes[name], int64(want.children[name].size); got != want {
			t.Errorf("%s: size %d, want %d", name, got, want)
		}
	}
}

func TestReadDirPlusServesStatWithoutLookup(t *testing.T) {
	maybeParallel(t)
	d := newReadDirPlusDir(20)
	readDirPlusStat(t, readDirPlusLookupDir{d}, d)
	if got := d.lookups.Count(); got != 0 {
		t.Errorf("stat after readdir made %d lookups, want 0", got)
	}
}

func TestReadDirPlusFallbackNodeIDZero(t *testing.T) {
	maybeParallel(t)
	d := newReadDirPlusDir(20)
	readDirPlusStat(t, d, d)
	if got := d.lookups.Count(); got != uint32(len(d.names)) {
		t.Errorf("stat after readdir made %d lookups, want one per entry, %d", got, len(d.names))
	}
}

// doReaddirDropCaches lists path, then drops the kernel's dentry and
// inode caches, which sends a FORGET for every inode the listing linked.
func doReaddirDropCaches(ctx context.Context, path string) (*struct{}, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if _, err := f.Readdirnames(-1); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("2"), 0); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

var readdirDropCachesHelper = helpers.Register("readdirDropCaches", httpjson.ServePOST(doReaddirDropCaches))

func TestReadDirPlusForgetBalances(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("dropping the kernel caches needs root")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newReadDirPlusDir(50)
	mnt, err := fstestutil.MountedT(t, fstestutil.SimpleFS{Node: readDirPlusLookupDir{d}}, nil, fuse.ReaddirPlus())
	if err != nil {
		t.Fatal(err)
	}
	defer mnt.Close()
	control := readdirDropCachesHelper.Spawn(ctx, t)
	defer control.Close()

	if err := control.JSON("/").Call(ctx, mnt.Dir, &struct{}{}); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	// FORGET arrives asynchronously after the caches are dropped.
	deadline := time.Now().Add(10 * time.Second)
	for d.forgets.Count() < uint32(len(d.names)) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := d.forgets.Count(); got != uint32(len(d.names)) {
		t.Errorf("%d of %d listed nodes were forgotten", got, len(d.names))
	}
	if got := d.lookups.Count(); got != 0 {
		t.Errorf("readdir made %d lookups, want 0", got)
	}
}
