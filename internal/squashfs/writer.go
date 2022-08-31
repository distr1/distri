// Package squashfs implements writing SquashFS file system images using zlib
// compression for data blocks (inodes and directory entries are written
// uncompressed for simplicity).
//
// Note that SquashFS requires directory entries to be sorted, i.e. files and
// directories need to be added in the correct order.
//
// This package intentionally only implements a subset of SquashFS. Notably,
// block devices, character devices, FIFOs, sockets and xattrs are not
// supported.
package squashfs

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// inode contains a block number + offset within that block.
type Inode int64

const (
	zlibCompression = 1 + iota
	lzmaCompression
	lzoCompression
	xzCompression
	lz4Compression
)

const (
	invalidFragment = 0xFFFFFFFF
	invalidXattr    = 0xFFFFFFFF
)

// Explanations partly copied from
// https://dr-emann.github.io/squashfs/squashfs.html#_the_superblock
type superblock struct {
	// Magic is always "hsqs"
	Magic uint32

	// Inodes is the number of inodes stored in the archive.
	Inodes uint32

	// MkfsTime is the last modification time of the archive, which is identical
	// to the creation time, since our archives are immutable.
	//
	// TODO: change this to uint32 as was done upstream in August 2019:
	// https://github.com/plougher/squashfs-tools/commit/66e9980f3e29ba59fdd7ab681197df1ff02916c7
	MkfsTime int32

	// BlockSize is the size of a data block in bytes.
	// Must be a power of two between 4 KiB and 1 MiB.
	BlockSize uint32

	// Fragments is the number of entries in the fragment table.
	Fragments uint32

	// Compression is an ID designating the compressor
	// used for both data and meta data blocks.
	//
	// TODO: define a custom uint16 type
	Compression uint16

	// The log_2 of the block size. If the two fields do not agree,
	// the archive is considered corrupted.
	BlockLog uint16

	// TODO: flag bit definitions?
	Flags uint16

	// NoIds is the number of entries in the ID lookup table.
	NoIds uint16

	// Major is the major version number (4).
	Major uint16

	// Minor is the minor version number (0).
	Minor uint16

	// RootInode is a reference to the inode of the root directory.
	RootInode Inode

	// BytesUsed is the number of bytes used by the archive.
	// Can be less than the actual file size because SquashFS
	// archives must be padded to a multiple of the underlying
	// device block size.
	// TODO: we currently only pad to 4k. what’s the largest underlying?
	BytesUsed int64

	// Byte offsets at which the respective id table starts.
	// If the xattr, fragment or export table are absent,
	// the respective field must be set to 0xFFFFFFFFFFFFFFFF.
	IdTableStart        int64
	XattrIdTableStart   int64
	InodeTableStart     int64
	DirectoryTableStart int64
	FragmentTableStart  int64
	LookupTableStart    int64
}

const (
	dirType = 1 + iota
	fileType
	symlinkType
	blkdevType
	chrdevType
	fifoType
	socketType
	// The larger types are used for e.g. sparse files, xattrs, etc.
	ldirType
	lregType
	lsymlinkType
	lblkdevType
	lchrdevType
	lfifoType
	lsocketType
)

// https://dr-emann.github.io/squashfs/squashfs.html#_common_inode_header
type inodeHeader struct {
	// TODO: define a custom uint16 type
	InodeType uint16

	// Mode is a bit mask representing Unix file permissions for the inode.
	// This only stores permissions, not the type. The type is reconstructed
	// from the InodeType field.
	Mode uint16

	// Uid is an index into the id table, giving the user id of the owner.
	Uid uint16

	// Gid is an index into the id table, giving the group id of the owner.
	Gid uint16

	// Mtime is the signed number of seconds since the UNIX epoch.
	//
	// TODO: change this to uint32 as was done upstream in August 2019:
	// https://github.com/plougher/squashfs-tools/commit/66e9980f3e29ba59fdd7ab681197df1ff02916c7
	Mtime int32

	// InodeNumber is a unique inode number.
	// Must be at least 1, at most the inode count from the super block.
	InodeNumber uint32
}

// fileType
//
// https://dr-emann.github.io/squashfs/squashfs.html#_file_inodes
type regInodeHeader struct {
	inodeHeader

	// StartBlock is the full byte offset from the start of the file system,
	// e.g. 96 for first file contents. Not using fragments limits us to
	// 2^32-1-96 (≈ 4GiB) bytes of file contents.
	StartBlock uint32

	// Fragment is an index into the fragment table which describes the fragment
	// block that the tail end of this file is stored in. If fragments are not
	// used, this field is set to 0xFFFFFFFF.
	Fragment uint32

	// Offset is the (uncompressed) offset within the fragment block where the
	// tail end of this file is.
	Offset uint32

	// FileSize is the (uncompressed) size of this file.
	FileSize uint32

	// Followed by a uint32 array of compressed block sizes.
	// See https://dr-emann.github.io/squashfs/squashfs.html#_data_and_fragment_blocks
}

// lregType
//
// https://dr-emann.github.io/squashfs/squashfs.html#_file_inodes
type lregInodeHeader struct {
	inodeHeader

	// StartBlock is the full byte offset from the start of the file system,
	// e.g. 96 for first file contents. Not using fragments limits us to
	// 2^32-1-96 (≈ 4GiB) bytes of file contents.
	StartBlock uint64

	// FileSize is the (uncompressed) size of this file.
	FileSize uint64

	// Sparse is the number of bytes saved by omitting zero bytes. Used in the
	// kernel for sparse file accounting.
	Sparse uint64

	// Nlink is the number of hard links to this node.
	Nlink uint32

	// Fragment is an index into the fragment table which describes the fragment
	// block that the tail end of this file is stored in. If fragments are not
	// used, this field is set to 0xFFFFFFFF.
	Fragment uint32

	// Offset is the (uncompressed) offset within the fragment block where the
	// tail end of this file is.
	Offset uint32

	// Xattr is an index into the Xattr table, or 0xFFFFFFFF if the inode has no
	// extended attributes.
	Xattr uint32

	// Followed by a uint32 array of compressed block sizes.
}

// symlinkType
//
// https://dr-emann.github.io/squashfs/squashfs.html#_symbolic_links
type symlinkInodeHeader struct {
	inodeHeader

	// Nlink is the number of hard links to this symlink.
	Nlink uint32

	// SymlinkSize is the size in bytes of the target path this symlink points
	// to.
	SymlinkSize uint32

	// Followed by a byte array of SymlinkSize bytes. The path is not
	// null-terminated.
}

// chrdevType and blkdevType
//
// https://dr-emann.github.io/squashfs/squashfs.html#_device_special_files
type devInodeHeader struct {
	inodeHeader

	// Nlink is the number of hard links to this entry.
	Nlink uint32

	// Rdev is the system-specific device number.
	Rdev uint32
}

// fifoType and socketType
//
// https://dr-emann.github.io/squashfs/squashfs.html#_ipc_inodes_fifo_or_socket
type ipcInodeHeader struct {
	inodeHeader

	// Nlink is the number of hard links to this entry.
	Nlink uint32
}

// dirType
//
// https://dr-emann.github.io/squashfs/squashfs.html#_directory_inodes
type dirInodeHeader struct {
	inodeHeader

	// StartBlock is the block index of the metadata block in the directory
	// table where the entry information starts. This is relative to the
	// directory table location.
	StartBlock uint32

	// Nlink is the number of hard links to this directory.
	Nlink uint32

	// FileSize is the total (uncompressed) size in bytes of the entry listing
	// in the directory table, including headers.
	//
	// This value is 3 bytes larger than the real listing. The Linux kernel
	// creates "." and ".." entries for offsets 0 and 1, and only after 3 looks
	// into the listing, subtracting 3 from the size.
	FileSize uint16

	// Offset is the (uncompressed) offset within the metadata block in the
	// directory table where the directory listing starts.
	Offset uint16

	// ParentInode is the inode number of the parent of this directory. If this
	// is the root directory, ParentInode should be 0.
	ParentInode uint32
}

// ldirType
//
// https://dr-emann.github.io/squashfs/squashfs.html#_directory_inodes
type ldirInodeHeader struct {
	inodeHeader

	// Nlink is the number of hard links to this directory.
	Nlink uint32

	// FileSize is the total (uncompressed) size in bytes of the entry listing
	// in the directory table, including headers.
	//
	// This value is 3 bytes larger than the real listing. The Linux kernel
	// creates "." and ".." entries for offsets 0 and 1, and only after 3 looks
	// into the listing, subtracting 3 from the size.
	FileSize uint32

	// StartBlock is the block index of the metadata block in the directory
	// table where the entry information starts. This is relative to the
	// directory table location.
	StartBlock uint32

	// ParentInode is the inode number of the parent of this directory. If this
	// is the root directory, ParentInode should be 0.
	ParentInode uint32

	// Icount is the number of directory index entries following this inode.
	Icount uint16

	// Offset is the (uncompressed) offset within the metadata block in the
	// directory table where the directory listing starts.
	Offset uint16

	// Xattr is an index into the Xattr table, or 0xFFFFFFFF if the inode has no
	// extended attributes.
	Xattr uint32
}

// https://dr-emann.github.io/squashfs/squashfs.html#_directory_table
type dirHeader struct {
	// Count is the number of entries following the header.
	Count uint32

	// StartBlock is the location of the metadata block in the inode table where
	// the inodes are stored. This is relative to the inode table start from the
	// super block.
	StartBlock uint32

	// InodeOffset is an arbitrary inode number. The entries that follow store
	// their inode number as a difference to this value.
	InodeOffset uint32
}

func (d *dirHeader) Unmarshal(b []byte) {
	_ = b[11]
	e := binary.LittleEndian
	d.Count = e.Uint32(b)
	d.StartBlock = e.Uint32(b[4:])
	d.InodeOffset = e.Uint32(b[8:])
}

// https://dr-emann.github.io/squashfs/squashfs.html#_directory_table
type dirEntry struct {
	// Offset is an offset into the uncompressed inode metadata block.
	Offset uint16

	// InodeNumber is the difference of this inode relative to dirHeader.InodeOffset.
	InodeNumber int16

	// EntryType is the inode type. For extended inodes, the basic type is
	// stored here instead.
	EntryType uint16

	// Size is one less than the size of the entry name.
	Size uint16

	// Followed by a byte array of Size+1 bytes.
}

func (d *dirEntry) Unmarshal(b []byte) {
	_ = b[7]
	e := binary.LittleEndian
	d.Offset = e.Uint16(b)
	d.InodeNumber = int16(e.Uint16(b[2:]))
	d.EntryType = e.Uint16(b[4:])
	d.Size = e.Uint16(b[6:])
}

// xattr types
const (
	XattrTypeUser = iota
	XattrTypeTrusted
	XattrTypeSecurity
)

var xattrPrefix = map[int]string{
	XattrTypeUser:     "user.",
	XattrTypeTrusted:  "trusted.",
	XattrTypeSecurity: "security.",
}

type Xattr struct {
	// Type is a prefix id for the key name. If the value that follows is stored
	// out-of-line, the flag 0x0100 is ORed to the type id.
	//
	// TODO: define a custom uint16 type
	Type uint16

	FullName string
	Value    []byte
}

func XattrFromAttr(attr string, val []byte) Xattr {
	for typ, prefix := range xattrPrefix {
		if !strings.HasPrefix(attr, prefix) {
			continue
		}
		return Xattr{
			Type:     uint16(typ),
			FullName: strings.TrimPrefix(attr, prefix),
			Value:    val,
		}
	}
	return Xattr{}
}

type xattrId struct {
	Xattr uint64
	Count uint32
	Size  uint32
}

func writeIdTable(w io.WriteSeeker, ids []uint32) (start int64, err error) {
	metaOff, err := w.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, ids); err != nil {
		return 0, err
	}

	if err := binary.Write(w, binary.LittleEndian, uint16(buf.Len())|0x8000); err != nil {
		return 0, err
	}
	if _, err := io.Copy(w, &buf); err != nil {
		return 0, err
	}
	off, err := w.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	return off, binary.Write(w, binary.LittleEndian, metaOff)
}

type fullDirEntry struct {
	startBlock  uint32
	offset      uint16
	inodeNumber uint32
	entryType   uint16
	name        string
}

const (
	magic             = 0x73717368
	dataBlockSize     = 131072
	metadataBlockSize = 8192
	majorVersion      = 4
	minorVersion      = 0
)

type Writer struct {
	// Root represents the file system root. Like all directories, Flush must be
	// called precisely once.
	Root *Directory

	xattrs   []Xattr
	xattrIds []xattrId

	w io.WriteSeeker

	sb       superblock
	inodeBuf bytes.Buffer
	dirBuf   bytes.Buffer

	writeInodeNumTo map[string][]int64
}

// TODO: document what this is doing and what it is used for
func slog(block uint32) uint16 {
	for i := uint16(12); i <= 20; i++ {
		if block == (1 << i) {
			return i
		}
	}
	return 0
}

// filesystemFlags returns flags for a SquashFS file system created by this
// package (disabling most features for now).
func filesystemFlags() uint16 {
	const (
		noI = 1 << iota // uncompressed metadata
		noD             // uncompressed data
		_
		noF               // uncompressed fragments
		noFrag            // never use fragments
		alwaysFrag        // always use fragments
		duplicateChecking // de-duplication
		exportable        // exportable via NFS
		noX               // uncompressed xattrs
		noXattr           // no xattrs
		compopt           // compressor-specific options present?
	)
	// TODO: is noXattr still accurate?
	return noI | noF | noFrag | noX | noXattr
}

// NewWriter returns a Writer which will write a SquashFS file system image to w
// once Flush is called.
//
// Create new files and directories with the corresponding methods on the Root
// directory of the Writer.
//
// File data is written to w even before Flush is called.
func NewWriter(w io.WriteSeeker, mkfsTime time.Time) (*Writer, error) {
	// Skip over superblock to the data area, we come back to the superblock
	// when flushing.
	if _, err := w.Seek(96, io.SeekStart); err != nil {
		return nil, err
	}
	wr := &Writer{
		w: w,
		sb: superblock{
			Magic:             magic,
			MkfsTime:          int32(mkfsTime.Unix()),
			BlockSize:         dataBlockSize,
			Fragments:         0,
			Compression:       zlibCompression,
			BlockLog:          slog(dataBlockSize),
			Flags:             filesystemFlags(),
			NoIds:             1, // just one uid/gid mapping (for root)
			Major:             majorVersion,
			Minor:             minorVersion,
			XattrIdTableStart: -1, // not present
			LookupTableStart:  -1, // not present
		},
		writeInodeNumTo: make(map[string][]int64),
	}
	wr.Root = &Directory{
		w:       wr,
		name:    "", // root
		modTime: mkfsTime,
	}
	return wr, nil
}

// Directory represents a SquashFS directory.
type Directory struct {
	w          *Writer
	name       string
	modTime    time.Time
	dirEntries []fullDirEntry
	parent     *Directory
}

func (d *Directory) path() string {
	if d.parent == nil {
		return d.name
	}
	return filepath.Join(d.parent.path(), d.name)
}

type file struct {
	w       *Writer
	d       *Directory
	off     int64
	size    uint32
	name    string
	modTime time.Time
	mode    uint16

	// buf accumulates at least dataBlockSize bytes, at which point a new block
	// is being written.
	buf bytes.Buffer

	// blocksizes stores, for each block of dataBlockSize bytes (uncompressed),
	// the number of bytes the block compressed down to.
	blocksizes []uint32

	// compBuf is used for holding a block during compression to avoid memory
	// allocations.
	compBuf *bytes.Buffer
	// zlibWriter is re-used for each compressed block
	zlibWriter *zlib.Writer

	xattrRef uint32
}

// Directory creates a new directory with the specified name and modTime.
func (d *Directory) Directory(name string, modTime time.Time) *Directory {
	return &Directory{
		w:       d.w,
		name:    name,
		modTime: modTime,
		parent:  d,
	}
}

// File creates a file with the specified name, modTime and mode. The returned
// io.WriterCloser must be closed after writing the file.
func (d *Directory) File(name string, modTime time.Time, mode uint16, xattrs []Xattr) (io.WriteCloser, error) {
	off, err := d.w.w.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}

	// zlib.BestSpeed results in only a 2x slow-down over no compression
	// (compared to >4x slow-down with DefaultCompression), but generates
	// results which are in the same ball park (10% larger).
	zw, err := zlib.NewWriterLevel(nil, zlib.BestSpeed)
	if err != nil {
		return nil, err
	}

	xattrRef := uint32(invalidXattr)
	if len(xattrs) > 0 {
		xattrRef = uint32(len(d.w.xattrs))
		d.w.xattrs = append(d.w.xattrs, xattrs[0]) // TODO: support multiple
		size := len(xattrs[0].FullName) + len(xattrs[0].Value)
		d.w.xattrIds = append(d.w.xattrIds, xattrId{
			// Xattr is populated in writeXattrTables
			Count: 1, // TODO: support multiple
			Size:  uint32(size),
		})
	}
	return &file{
		w:          d.w,
		d:          d,
		off:        off,
		name:       name,
		modTime:    modTime,
		mode:       mode,
		compBuf:    bytes.NewBuffer(make([]byte, dataBlockSize)),
		zlibWriter: zw,
		xattrRef:   xattrRef,
	}, nil
}

// Symlink creates a symbolic link from newname to oldname with the specified
// modTime and mode.
func (d *Directory) Symlink(oldname, newname string, modTime time.Time, mode os.FileMode) error {
	startBlock := d.w.inodeBuf.Len() / metadataBlockSize
	offset := d.w.inodeBuf.Len() - startBlock*metadataBlockSize

	if err := binary.Write(&d.w.inodeBuf, binary.LittleEndian, symlinkInodeHeader{
		inodeHeader: inodeHeader{
			InodeType:   symlinkType,
			Mode:        uint16(mode),
			Uid:         0,
			Gid:         0,
			Mtime:       int32(modTime.Unix()),
			InodeNumber: d.w.sb.Inodes + 1,
		},
		Nlink:       1, // TODO(later): when is this not 1?
		SymlinkSize: uint32(len(oldname)),
	}); err != nil {
		return err
	}
	if _, err := d.w.inodeBuf.Write([]byte(oldname)); err != nil {
		return err
	}

	d.dirEntries = append(d.dirEntries, fullDirEntry{
		startBlock:  uint32(startBlock),
		offset:      uint16(offset),
		inodeNumber: d.w.sb.Inodes + 1,
		entryType:   symlinkType,
		name:        newname,
	})

	d.w.sb.Inodes++
	return nil
}

// Flush writes directory entries and creates inodes for the directory.
func (d *Directory) Flush() error {
	countByStartBlock := make(map[uint32]uint32)
	for _, de := range d.dirEntries {
		countByStartBlock[de.startBlock]++
	}

	dirBufStartBlock := d.w.dirBuf.Len() / metadataBlockSize
	dirBufOffset := d.w.dirBuf.Len()

	currentBlock := int64(-1)
	currentInodeOffset := int64(-1)
	var subdirs int
	for _, de := range d.dirEntries {
		if de.entryType == dirType {
			subdirs++
		}
		if int64(de.startBlock) != currentBlock {
			dh := dirHeader{
				Count:       countByStartBlock[de.startBlock] - 1,
				StartBlock:  de.startBlock * (metadataBlockSize + 2),
				InodeOffset: de.inodeNumber,
			}
			if err := binary.Write(&d.w.dirBuf, binary.LittleEndian, &dh); err != nil {
				return err
			}

			currentBlock = int64(de.startBlock)
			currentInodeOffset = int64(de.inodeNumber)
		}
		if err := binary.Write(&d.w.dirBuf, binary.LittleEndian, &dirEntry{
			Offset:      de.offset,
			InodeNumber: int16(de.inodeNumber - uint32(currentInodeOffset)),
			EntryType:   de.entryType,
			Size:        uint16(len(de.name) - 1),
		}); err != nil {
			return err
		}
		if _, err := d.w.dirBuf.Write([]byte(de.name)); err != nil {
			return err
		}
	}

	startBlock := d.w.inodeBuf.Len() / metadataBlockSize
	offset := d.w.inodeBuf.Len() - startBlock*metadataBlockSize
	inodeBufOffset := d.w.inodeBuf.Len()

	// parentInodeOffset is the offset (in bytes) of the ParentInode field
	// within a dirInodeHeader or ldirInodeHeader
	var parentInodeOffset int64

	if len(d.dirEntries) > 256 ||
		d.w.dirBuf.Len()-dirBufOffset > metadataBlockSize {
		parentInodeOffset = (2 + 2 + 2 + 2 + 4 + 4) + 4 + 4 + 4
		if err := binary.Write(&d.w.inodeBuf, binary.LittleEndian, ldirInodeHeader{
			inodeHeader: inodeHeader{
				InodeType: ldirType,
				Mode: unix.S_IRUSR | unix.S_IWUSR | unix.S_IXUSR |
					unix.S_IRGRP | unix.S_IXGRP |
					unix.S_IROTH | unix.S_IXOTH,
				Uid:         0,
				Gid:         0,
				Mtime:       int32(d.modTime.Unix()),
				InodeNumber: d.w.sb.Inodes + 1,
			},

			Nlink:       uint32(subdirs + 2 - 1), // + 2 for . and ..
			FileSize:    uint32(d.w.dirBuf.Len()-dirBufOffset) + 3,
			StartBlock:  uint32(dirBufStartBlock * (metadataBlockSize + 2)),
			ParentInode: d.w.sb.Inodes + 2, // invalid
			Icount:      0,                 // no directory index
			Offset:      uint16(dirBufOffset - dirBufStartBlock*metadataBlockSize),
			Xattr:       invalidXattr,
		}); err != nil {
			return err
		}
	} else {
		parentInodeOffset = (2 + 2 + 2 + 2 + 4 + 4) + 4 + 4 + 2 + 2
		if err := binary.Write(&d.w.inodeBuf, binary.LittleEndian, dirInodeHeader{
			inodeHeader: inodeHeader{
				InodeType: dirType,
				Mode: unix.S_IRUSR | unix.S_IWUSR | unix.S_IXUSR |
					unix.S_IRGRP | unix.S_IXGRP |
					unix.S_IROTH | unix.S_IXOTH,
				Uid:         0,
				Gid:         0,
				Mtime:       int32(d.modTime.Unix()),
				InodeNumber: d.w.sb.Inodes + 1,
			},
			StartBlock:  uint32(dirBufStartBlock * (metadataBlockSize + 2)),
			Nlink:       uint32(subdirs + 2 - 1), // + 2 for . and ..
			FileSize:    uint16(d.w.dirBuf.Len()-dirBufOffset) + 3,
			Offset:      uint16(dirBufOffset - dirBufStartBlock*metadataBlockSize),
			ParentInode: d.w.sb.Inodes + 2, // invalid
		}); err != nil {
			return err
		}
	}

	path := d.path()
	for _, offset := range d.w.writeInodeNumTo[path] {
		// Directly manipulating unread data in bytes.Buffer via Bytes(), as per
		// https://groups.google.com/d/msg/golang-nuts/1ON9XVQ1jXE/8j9RaeSYxuEJ
		b := d.w.inodeBuf.Bytes()
		binary.LittleEndian.PutUint32(b[offset:offset+4], d.w.sb.Inodes+1)
	}

	if d.parent != nil {
		parentPath := filepath.Dir(d.path())
		if parentPath == "." {
			parentPath = ""
		}
		d.w.writeInodeNumTo[parentPath] = append(d.w.writeInodeNumTo[parentPath], int64(inodeBufOffset)+parentInodeOffset)
		d.parent.dirEntries = append(d.parent.dirEntries, fullDirEntry{
			startBlock:  uint32(startBlock),
			offset:      uint16(offset),
			inodeNumber: d.w.sb.Inodes + 1,
			entryType:   dirType,
			name:        d.name,
		})
	} else { // root
		d.w.sb.RootInode = Inode((startBlock*(metadataBlockSize+2))<<16 | offset)
	}

	d.w.sb.Inodes++

	return nil
}

// Write implements io.Writer
func (f *file) Write(p []byte) (n int, err error) {
	n, err = f.buf.Write(p)
	if n > 0 {
		// Keep track of the uncompressed file size.
		f.size += uint32(n)
		for f.buf.Len() >= dataBlockSize {
			if err := f.writeBlock(); err != nil {
				return 0, err
			}
		}
	}
	return n, err
}

func (f *file) writeBlock() error {
	n := f.buf.Len()
	if n > dataBlockSize {
		n = dataBlockSize
	}
	// Feed dataBlockSize bytes to the compressor
	b := f.buf.Bytes()
	block := b[:n]
	rest := b[n:]
	/*
		f.compBuf.Reset()
		f.zlibWriter.Reset(f.compBuf)
		if _, err := f.zlibWriter.Write(block); err != nil {
			return err
		}
		if err := f.zlibWriter.Close(); err != nil {
			return err
		}

		size := f.compBuf.Len()
		if size > len(block) {
			// Copy uncompressed data: Linux returns i/o errors when it encounters a
			// compressed block which is larger than the uncompressed data:
			// https://github.com/torvalds/linux/blob/3ca24ce9ff764bc27bceb9b2fd8ece74846c3fd3/fs/squashfs/block.c#L150
			size = len(block) | (1 << 24) // SQUASHFS_COMPRESSED_BIT_BLOCK
			if _, err := f.w.w.Write(block); err != nil {
				return err
			}
		} else {
			if _, err := io.Copy(f.w.w, f.compBuf); err != nil {
				return err
			}
		}
	*/
	// Copy uncompressed data: Linux returns i/o errors when it encounters a
	// compressed block which is larger than the uncompressed data:
	// https://github.com/torvalds/linux/blob/3ca24ce9ff764bc27bceb9b2fd8ece74846c3fd3/fs/squashfs/block.c#L150
	size := len(block) | (1 << 24) // SQUASHFS_COMPRESSED_BIT_BLOCK
	if _, err := f.w.w.Write(block); err != nil {
		return err
	}

	f.blocksizes = append(f.blocksizes, uint32(size))

	// Keep the rest in f.buf for the next write
	copy(b, rest)
	f.buf.Truncate(len(rest))
	return nil
}

// Close implements io.Closer
func (f *file) Close() error {
	for f.buf.Len() > 0 {
		if err := f.writeBlock(); err != nil {
			return err
		}
	}

	startBlock := f.w.inodeBuf.Len() / metadataBlockSize
	offset := f.w.inodeBuf.Len() - startBlock*metadataBlockSize

	if err := binary.Write(&f.w.inodeBuf, binary.LittleEndian, lregInodeHeader{
		inodeHeader: inodeHeader{
			InodeType:   lregType,
			Mode:        f.mode,
			Uid:         0,
			Gid:         0,
			Mtime:       int32(f.modTime.Unix()),
			InodeNumber: f.w.sb.Inodes + 1,
		},
		StartBlock: uint64(f.off),
		FileSize:   uint64(f.size),
		Nlink:      1,
		Fragment:   invalidFragment,
		Offset:     0,
		Xattr:      f.xattrRef,
	}); err != nil {
		return err
	}

	if err := binary.Write(&f.w.inodeBuf, binary.LittleEndian, f.blocksizes); err != nil {
		return err
	}

	f.d.dirEntries = append(f.d.dirEntries, fullDirEntry{
		startBlock:  uint32(startBlock),
		offset:      uint16(offset),
		inodeNumber: f.w.sb.Inodes + 1,
		entryType:   fileType,
		name:        f.name,
	})

	f.w.sb.Inodes++

	return nil
}

// https://dr-emann.github.io/squashfs/squashfs.html#_xattr_table
func writeXattr(w io.Writer, xattrs []Xattr) error {
	for _, attr := range xattrs {
		if err := binary.Write(w, binary.LittleEndian, struct {
			Type     uint16
			NameSize uint16
		}{
			Type:     attr.Type,
			NameSize: uint16(len(attr.FullName)),
		}); err != nil {
			return err
		}
		if _, err := w.Write([]byte(attr.FullName)); err != nil {
			return err
		}

		if err := binary.Write(w, binary.LittleEndian, struct {
			ValSize uint32
		}{
			ValSize: uint32(len(attr.Value)),
		}); err != nil {
			return err
		}

		if _, err := w.Write(attr.Value); err != nil {
			return err
		}
	}
	return nil
}

type xattrTableHeader struct {
	XattrTableStart uint64
	XattrIds        uint32
	Unused          uint32
}

func (w *Writer) writeXattrTables() (int64, error) {
	if len(w.xattrs) == 0 {
		return -1, nil
	}
	off, err := w.w.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	xattrTableStart := uint64(off)

	var xattrBuf bytes.Buffer
	if err := writeXattr(&xattrBuf, w.xattrs); err != nil {
		return 0, err
	}
	xattrBlocks := (xattrBuf.Len() + (metadataBlockSize - 1)) / metadataBlockSize

	if err := w.writeMetadataChunks(&xattrBuf); err != nil {
		return 0, err
	}

	// write xattr id table
	off, err = w.w.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	idTableOff := uint64(off)
	var xattrIdBuf bytes.Buffer
	size := uint64(0)
	for _, id := range w.xattrIds {
		id.Xattr = uint64(size)
		size += uint64(id.Size) + 8 /* sizeof(Type+NameSize+ValSize) */
		if err := binary.Write(&xattrIdBuf, binary.LittleEndian, id); err != nil {
			return 0, err
		}
	}
	if err := w.writeMetadataChunks(&xattrIdBuf); err != nil {
		return 0, err
	}

	// xattr table header
	off, err = w.w.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if err := binary.Write(w.w, binary.LittleEndian, xattrTableHeader{
		XattrTableStart: xattrTableStart,
		XattrIds:        uint32(len(w.xattrs)),
	}); err != nil {
		return 0, err
	}
	// write block index
	for i := 0; i < xattrBlocks; i++ {
		if err := binary.Write(w.w, binary.LittleEndian, struct {
			BlockOffset uint64
		}{
			BlockOffset: idTableOff + (uint64(i) * (8192 + 2 /* sizeof(uint16) */)),
		}); err != nil {
			return 0, err
		}
	}
	return off, nil
}

// writeMetadataChunks copies from r to w in blocks of metadataBlockSize bytes
// each, prefixing each block with a uint16 length header, setting the
// uncompressed bit.
func (w *Writer) writeMetadataChunks(r io.Reader) error {
	buf := make([]byte, metadataBlockSize)
	for {
		buf = buf[:metadataBlockSize]
		n, err := r.Read(buf)
		if err != nil {
			if err == io.EOF { // done
				return nil
			}
			return err
		}
		buf = buf[:n]
		if err := binary.Write(w.w, binary.LittleEndian, uint16(len(buf))|0x8000); err != nil {
			return err
		}
		if _, err := w.w.Write(buf); err != nil {
			return err
		}
	}
}

// Flush writes the SquashFS file system. The Writer must not be used after
// calling Flush.
func (w *Writer) Flush() error {
	// (1) superblock will be written later

	// (2) compressor-specific options omitted

	// (3) data has already been written

	// (4) write inode table
	off, err := w.w.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	w.sb.InodeTableStart = off

	if err := w.writeMetadataChunks(&w.inodeBuf); err != nil {
		return err
	}

	// (5) write directory table
	off, err = w.w.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	w.sb.DirectoryTableStart = off

	if err := w.writeMetadataChunks(&w.dirBuf); err != nil {
		return err
	}

	// (6) fragment table omitted
	off, err = w.w.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	w.sb.FragmentTableStart = off

	// (7) export table omitted

	// (8) write uid/gid lookup table
	idTableStart, err := writeIdTable(w.w, []uint32{0})
	if err != nil {
		return err
	}
	w.sb.IdTableStart = idTableStart

	// (9) xattr table
	off, err = w.writeXattrTables()
	if err != nil {
		return err
	}
	w.sb.XattrIdTableStart = off

	off, err = w.w.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	w.sb.BytesUsed = off

	// Pad to 4096, required for the kernel to be able to access all pages
	if pad := off % 4096; pad > 0 {
		padding := make([]byte, 4096-pad)
		if _, err := w.w.Write(padding); err != nil {
			return err
		}
	}

	// (1) Write superblock
	if _, err := w.w.Seek(0, io.SeekStart); err != nil {
		return err
	}

	return binary.Write(w.w, binary.LittleEndian, &w.sb)
}
