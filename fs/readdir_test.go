package fs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"
	"testing"

	"bazil.org/fuse"
)

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
		off := binary.LittleEndian.Uint64(buf[8:16])
		namelen := int(binary.LittleEndian.Uint32(buf[16:20]))
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
