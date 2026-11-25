package fs

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"bazil.org/fuse"
)

// The encoders and the kernel write dirents in the host's byte order.
var hostEndian binary.ByteOrder = binary.LittleEndian

func init() {
	one := uint16(1)
	if *(*byte)(unsafe.Pointer(&one)) == 0 {
		hostEndian = binary.BigEndian
	}
}

type decodedDirent struct {
	name string
	off  uint64
}

// decodeDirents parses the records appendDirents wrote and fails the
// test on a partial one.
func decodeDirents(t *testing.T, buf []byte) []decodedDirent {
	t.Helper()
	var out []decodedDirent
	for len(buf) > 0 {
		if len(buf) < 24 {
			t.Fatalf("partial record header: %d bytes left", len(buf))
		}
		off := hostEndian.Uint64(buf[8:16])
		namelen := int(hostEndian.Uint32(buf[16:20]))
		reclen := (24 + namelen + 7) &^ 7
		if len(buf) < reclen {
			t.Fatalf("partial record: %d bytes left, record is %d", len(buf), reclen)
		}
		out = append(out, decodedDirent{name: string(buf[24 : 24+namelen]), off: off})
		buf = buf[reclen:]
	}
	return out
}

func testDirents(n int) []fuse.Dirent {
	dirents := make([]fuse.Dirent, n)
	for i := range dirents {
		dirents[i] = fuse.Dirent{Name: fmt.Sprintf("entry-%04d", i), Inode: uint64(100 + i), Type: fuse.DT_File}
	}
	return dirents
}

func TestAppendDirentsOffsetsAreIndexes(t *testing.T) {
	dirents := testDirents(3)
	buf, err := appendDirents(nil, dirents, 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeDirents(t, buf)
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3", len(got))
	}
	for i, d := range got {
		if d.name != dirents[i].Name || d.off != uint64(i+1) {
			t.Errorf("record %d: %q off %d, want %q off %d", i, d.name, d.off, dirents[i].Name, i+1)
		}
	}
}

func TestAppendDirentsOffsetOutsideListIsEnd(t *testing.T) {
	dirents := testDirents(3)
	for _, offset := range []int64{-1, 3, 4, 1 << 40} {
		buf, err := appendDirents(nil, dirents, offset, 4096)
		if err != nil || len(buf) != 0 {
			t.Errorf("offset %d: %d bytes, err %v; want an empty reply", offset, len(buf), err)
		}
	}
}

// Replies of a size that holds only some records, resumed at the last
// record's offset as the kernel does, return every name once, in order.
func TestAppendDirentsResumesAcrossReplies(t *testing.T) {
	dirents := testDirents(10)
	record := (24 + len(dirents[0].Name) + 7) &^ 7
	for _, size := range []int{record, record + 1, 3 * record, 3*record - 1} {
		var names []string
		offset := int64(0)
		for {
			buf, err := appendDirents(nil, dirents, offset, size)
			if err != nil {
				t.Fatalf("size %d offset %d: %v", size, offset, err)
			}
			if len(buf) > size {
				t.Fatalf("size %d: reply of %d bytes", size, len(buf))
			}
			got := decodeDirents(t, buf)
			if len(got) == 0 {
				break
			}
			for _, d := range got {
				names = append(names, d.name)
			}
			offset = int64(got[len(got)-1].off)
		}
		if len(names) != len(dirents) {
			t.Fatalf("size %d: got %d names, want %d: %q", size, len(names), len(dirents), names)
		}
		for i, name := range names {
			if name != dirents[i].Name {
				t.Fatalf("size %d: name %d is %q, want %q", size, i, name, dirents[i].Name)
			}
		}
	}
}

func TestAppendDirentsFirstRecordTooLarge(t *testing.T) {
	dirents := testDirents(2)
	record := (24 + len(dirents[0].Name) + 7) &^ 7
	buf, err := appendDirents(nil, dirents, 0, record-1)
	if !errors.Is(err, syscall.EINVAL) || len(buf) != 0 {
		t.Fatalf("got %d bytes, err %v; want an empty reply and EINVAL", len(buf), err)
	}
}

func TestAppendDirentsAppendsAfterExistingData(t *testing.T) {
	dirents := testDirents(2)
	record := (24 + len(dirents[0].Name) + 7) &^ 7
	prefix := []byte("prefix")
	buf, err := appendDirents(prefix, dirents, 0, record)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:len(prefix)]) != "prefix" {
		t.Fatalf("prefix overwritten: %q", buf[:len(prefix)])
	}
	if got := decodeDirents(t, buf[len(prefix):]); len(got) != 1 || got[0].name != dirents[0].Name {
		t.Fatalf("got %v, want only %q", got, dirents[0].Name)
	}
}

var testProto = fuse.Protocol{Major: 7, Minor: 33}

type plusRecord struct {
	nodeID     uint64
	generation uint64
	size       uint64
	mode       uint32
	name       string
	off        uint64
}

// decodeDirentsPlus parses READDIRPLUS records and fails the test on a
// partial one.
func decodeDirentsPlus(t *testing.T, buf []byte) []plusRecord {
	t.Helper()
	entrySize := fuse.DirentPlusSize(fuse.Dirent{}, testProto) - 24
	var out []plusRecord
	for len(buf) > 0 {
		if len(buf) < entrySize+24 {
			t.Fatalf("partial record header: %d bytes left", len(buf))
		}
		d := buf[entrySize:]
		namelen := int(binary.LittleEndian.Uint32(d[16:20]))
		reclen := entrySize + (24+namelen+7)&^7
		if len(buf) < reclen {
			t.Fatalf("partial record: %d bytes left, record is %d", len(buf), reclen)
		}
		out = append(out, plusRecord{
			nodeID:     binary.LittleEndian.Uint64(buf[0:8]),
			generation: binary.LittleEndian.Uint64(buf[8:16]),
			size:       binary.LittleEndian.Uint64(buf[48:56]),
			mode:       binary.LittleEndian.Uint32(buf[100:104]),
			name:       string(d[24 : 24+namelen]),
			off:        binary.LittleEndian.Uint64(d[8:16]),
		})
		buf = buf[reclen:]
	}
	return out
}

type plusTestNode struct {
	size    uint64
	forgets int
}

func (n *plusTestNode) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0o644
	a.Size = n.size
	return nil
}

func (n *plusTestNode) Forget() { n.forgets++ }

// plusTestLookup answers LookupPlus for the names in nodes only.
type plusTestLookup struct {
	nodes map[string]*plusTestNode
}

func (l *plusTestLookup) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) { return nil, nil }

func (l *plusTestLookup) LookupPlus(ctx context.Context, name string, attr *fuse.Attr) (Node, bool) {
	n, ok := l.nodes[name]
	if !ok {
		return nil, false
	}
	if err := n.Attr(ctx, attr); err != nil {
		return nil, false
	}
	return n, true
}

// newPlusTestServer is a Server with the node table Serve sets up: no
// node 0, the root at 1.
func newPlusTestServer() (*Server, *serveNode) {
	c := New(nil, nil)
	root := &serveNode{inode: 1, refs: 1}
	c.node = append(c.node, nil, root)
	return c, root
}

func (c *Server) testRefs(n Node) uint64 {
	c.meta.Lock()
	defer c.meta.Unlock()
	id, ok := c.nodeRef[n]
	if !ok {
		return 0
	}
	return c.node[id].refs
}

func TestAppendDirentsPlusRecords(t *testing.T) {
	c, root := newPlusTestServer()
	dirents := testDirents(3)
	lookup := &plusTestLookup{nodes: map[string]*plusTestNode{
		dirents[0].Name: {size: 10},
		dirents[2].Name: {size: 12},
	}}
	buf, err := c.appendDirentsPlus(context.Background(), nil, dirents, 0, 4096, testProto, root, lookup)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeDirentsPlus(t, buf)
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3", len(got))
	}
	for i, r := range got {
		if r.name != dirents[i].Name || r.off != uint64(i+1) {
			t.Errorf("record %d: %q off %d, want %q off %d", i, r.name, r.off, dirents[i].Name, i+1)
		}
	}
	for _, i := range []int{0, 2} {
		r := got[i]
		if r.nodeID == 0 || r.size != uint64(10+i) || r.mode&syscall.S_IFMT != syscall.S_IFREG {
			t.Errorf("record %d: node %d size %d mode %o; want a node, size %d, a regular file", i, r.nodeID, r.size, r.mode, 10+i)
		}
		if refs := c.testRefs(lookup.nodes[r.name]); refs != 1 {
			t.Errorf("record %d: node holds %d references, want 1", i, refs)
		}
	}
	if r := got[1]; r.nodeID != 0 || r.size != 0 || r.mode != 0 {
		t.Errorf("record 1: node %d size %d mode %o; want an entry without attributes", r.nodeID, r.size, r.mode)
	}
}

func TestAppendDirentsPlusWithoutLookupTakesNoReferences(t *testing.T) {
	c, root := newPlusTestServer()
	buf, err := c.appendDirentsPlus(context.Background(), nil, testDirents(3), 0, 4096, testProto, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range decodeDirentsPlus(t, buf) {
		if r.nodeID != 0 {
			t.Errorf("record %d: node %d, want 0", i, r.nodeID)
		}
	}
	if len(c.node) != 2 {
		t.Errorf("node table has %d slots, want the 2 of nil and the root", len(c.node))
	}
}

// The kernel adds one lookup reference for every record it links, also
// for a record it already got in an earlier reply.
func TestAppendDirentsPlusRepeatedRecordTakesAReferencePerSend(t *testing.T) {
	c, root := newPlusTestServer()
	dirents := testDirents(1)
	node := &plusTestNode{size: 1}
	lookup := &plusTestLookup{nodes: map[string]*plusTestNode{dirents[0].Name: node}}
	var ids []uint64
	for send := 0; send < 2; send++ {
		buf, err := c.appendDirentsPlus(context.Background(), nil, dirents, 0, 4096, testProto, root, lookup)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, decodeDirentsPlus(t, buf)[0].nodeID)
	}
	if ids[0] != ids[1] {
		t.Errorf("node IDs %v, want the same node twice", ids)
	}
	if refs := c.testRefs(node); refs != 2 {
		t.Errorf("node holds %d references, want 2", refs)
	}
}

// The kernel never links "." or "..", so it would never return a
// reference taken for them.
func TestAppendDirentsPlusDotEntriesTakeNoReferences(t *testing.T) {
	c, root := newPlusTestServer()
	dirents := []fuse.Dirent{{Name: ".", Inode: 10}, {Name: "..", Inode: 11}, {Name: "a", Inode: 12}}
	lookup := &plusTestLookup{nodes: map[string]*plusTestNode{".": {size: 1}, "..": {size: 2}, "a": {size: 3}}}
	buf, err := c.appendDirentsPlus(context.Background(), nil, dirents, 0, 4096, testProto, root, lookup)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeDirentsPlus(t, buf)
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3", len(got))
	}
	for _, r := range got[:2] {
		if r.nodeID != 0 || c.testRefs(lookup.nodes[r.name]) != 0 {
			t.Errorf("%q: node %d with %d references; want an entry without attributes", r.name, r.nodeID, c.testRefs(lookup.nodes[r.name]))
		}
	}
	if r := got[2]; r.nodeID == 0 || c.testRefs(lookup.nodes["a"]) != 1 {
		t.Errorf("%q: node %d with %d references; want a node with 1", r.name, r.nodeID, c.testRefs(lookup.nodes["a"]))
	}
}

func TestAppendDirentsPlusInvalidNameFailsAndReleases(t *testing.T) {
	for _, bad := range []string{"", "a/b", "a\x00b", strings.Repeat("x", maxDirentPlusName+1)} {
		c, root := newPlusTestServer()
		node := &plusTestNode{size: 1}
		dirents := []fuse.Dirent{{Name: "good", Inode: 10}, {Name: bad, Inode: 11}}
		lookup := &plusTestLookup{nodes: map[string]*plusTestNode{"good": node}}
		buf, err := c.appendDirentsPlus(context.Background(), nil, dirents, 0, 1<<20, testProto, root, lookup)
		if !errors.Is(err, syscall.EIO) || len(buf) != 0 {
			t.Errorf("name of %d bytes: got %d bytes, err %v; want an empty reply and EIO", len(bad), len(buf), err)
		}
		if refs := c.testRefs(node); refs != 0 || node.forgets != 1 {
			t.Errorf("name of %d bytes: earlier node holds %d references, forgotten %d times; want released and forgotten once", len(bad), refs, node.forgets)
		}
	}
}

func TestAppendDirentsPlusWholeRecordsOnly(t *testing.T) {
	dirents := testDirents(10)
	nodes := map[string]*plusTestNode{}
	for _, d := range dirents {
		nodes[d.Name] = &plusTestNode{size: 1}
	}
	record := fuse.DirentPlusSize(dirents[0], testProto)

	c, root := newPlusTestServer()
	buf, err := c.appendDirentsPlus(context.Background(), nil, dirents, 0, record-1, testProto, root, &plusTestLookup{nodes: nodes})
	if !errors.Is(err, syscall.EINVAL) || len(buf) != 0 {
		t.Fatalf("request smaller than a record: got %d bytes, err %v; want an empty reply and EINVAL", len(buf), err)
	}
	if refs := c.testRefs(nodes[dirents[0].Name]); refs != 0 {
		t.Fatalf("a record that did not fit took %d references", refs)
	}

	for _, size := range []int{record, 3*record - 1, 3 * record} {
		c, root := newPlusTestServer()
		lookup := &plusTestLookup{nodes: nodes}
		var names []string
		offset := int64(0)
		for {
			buf, err := c.appendDirentsPlus(context.Background(), nil, dirents, offset, size, testProto, root, lookup)
			if err != nil {
				t.Fatalf("size %d offset %d: %v", size, offset, err)
			}
			if len(buf) > size {
				t.Fatalf("size %d: reply of %d bytes", size, len(buf))
			}
			got := decodeDirentsPlus(t, buf)
			if len(got) == 0 {
				break
			}
			for _, r := range got {
				names = append(names, r.name)
			}
			offset = int64(got[len(got)-1].off)
		}
		if len(names) != len(dirents) {
			t.Fatalf("size %d: got %d names, want %d", size, len(names), len(dirents))
		}
		for i, d := range dirents {
			if names[i] != d.Name {
				t.Fatalf("size %d: name %d is %q, want %q", size, i, names[i], d.Name)
			}
			if refs := c.testRefs(nodes[d.Name]); refs != 1 {
				t.Fatalf("size %d: %q holds %d references, want 1", size, d.Name, refs)
			}
		}
	}
}

func TestDirentPlusSize(t *testing.T) {
	dir := fuse.Dirent{Name: "abc"}
	for _, minor := range []uint32{21, 33} {
		if got, want := fuse.DirentPlusSize(dir, fuse.Protocol{Major: 7, Minor: minor}), 128+32; got != want {
			t.Errorf("protocol 7.%d: %d, want %d", minor, got, want)
		}
	}
}
