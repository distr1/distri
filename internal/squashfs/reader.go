package squashfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"syscall"
	"time"
)

type Reader struct {
	r     io.ReaderAt
	super superblock
}

func NewReader(r io.ReaderAt) (*Reader, error) {
	var sb superblock

	if err := binary.Read(io.NewSectionReader(r, 0, int64(binary.Size(sb))), binary.LittleEndian, &sb); err != nil {
		return nil, fmt.Errorf("reading superblock: %v", err)
	}

	if got, want := sb.Magic, uint32(magic); got != want {
		return nil, fmt.Errorf("invalid magic (not a SquashFS image?): got %x, want %x", got, want)
	}

	//log.Printf("superblock: %+v", sb)
	return &Reader{
		r:     r,
		super: sb,
	}, nil
}

// TODO: maybe mmap instead of seeking?

func (r *Reader) inode(i Inode) (blockoffset int64, offset int64) {
	return int64(i >> 16), int64(i & 0xFFFF)
}

type blockReader struct {
	r   io.ReadSeeker
	buf *bytes.Buffer

	off int64 // TODO: remove this once using mmap
}

func (br *blockReader) Read(p []byte) (n int, err error) {
	n, err = br.buf.Read(p)
	//log.Printf("n = %v, err = %v", n, err)
	if err == io.EOF {
		br.buf.Reset()
		var l uint16
		if err := binary.Read(br.r, binary.LittleEndian, &l); err != nil {
			return 0, err
		}
		//uncompressed := l&0x8000 > 0
		l &= 0x7FFF
		//log.Printf("block of len %d, uncompressed: %v", l, uncompressed)
		if _, err := io.CopyN(br.buf, br.r, int64(l)); err != nil {
			return 0, err
		}
		n, err = br.buf.Read(p)
		//log.Printf("(retry) n = %v, err = %v", n, err)
	}
	return n, err
}

func (r *Reader) blockReader(blockoffset, offset int64) (io.Reader, error) {
	//log.Printf("blockoffset %v (%x), offset %v (%x)", blockoffset, blockoffset, offset, offset)
	br := &blockReader{
		r:   io.NewSectionReader(r.r, blockoffset, 5500*1024*1024), // TODO: correct limit? can we use IntMax
		buf: bytes.NewBuffer(make([]byte, 0, metadataBlockSize)),
		off: blockoffset,
	}
	//log.Printf("discarding %d bytes", offset)
	if _, err := io.CopyN(ioutil.Discard, br, offset); err != nil {
		return nil, err
	}
	return br, nil
}

// TODO: define an inode type to use instead of interface{}?
func (r *Reader) readInode(i Inode) (interface{}, error) {
	blockoffset, offset := r.inode(i)
	br, err := r.blockReader(r.super.InodeTableStart+blockoffset, offset)
	if err != nil {
		return nil, err
	}

	// We need the inode type before we know which type to pass to binary.Read,
	// so we need to read it twice:
	var inodeType uint16
	typeBuf := bytes.NewBuffer(make([]byte, 0, binary.Size(inodeType)))
	if err := binary.Read(io.TeeReader(br, typeBuf), binary.LittleEndian, &inodeType); err != nil {
		return nil, err
	}
	br = io.MultiReader(typeBuf, br)

	// var ih inodeHeader
	// if err := binary.Read(br, binary.LittleEndian, &ih); err != nil {
	// 	return err
	// }
	// //log.Printf("ih: %+v", ih)

	//log.Printf("inode type: %v", inodeType)
	switch inodeType {
	case dirType:
		var di dirInodeHeader
		if err := binary.Read(br, binary.LittleEndian, &di); err != nil {
			return nil, err
		}
		return di, nil

	case fileType:
		var ri regInodeHeader
		if err := binary.Read(br, binary.LittleEndian, &ri); err != nil {
			return nil, err
		}
		return ri, nil

	case symlinkType:
		var si symlinkInodeHeader
		if err := binary.Read(br, binary.LittleEndian, &si); err != nil {
			return nil, err
		}
		return si, nil

	case ldirType:
		var di ldirInodeHeader
		if err := binary.Read(br, binary.LittleEndian, &di); err != nil {
			return nil, err
		}
		return di, nil

		// TODO:
		// blkdevType
		// chrdevType
		// fifoType
		// socketType
		// // The larger types are used for e.g. sparse files, xattrs, etc.
		// ldirType
		// lregType
		// lsymlinkType
		// lblkdevType
		// lchrdevType
		// lfifoType
		// lsocketType

	}
	return nil, fmt.Errorf("unknown inode type %d", inodeType)
}

func (r *Reader) RootInode() Inode {
	return r.super.RootInode
}

func (r *Reader) Stat(name string, i Inode) (os.FileInfo, error) {
	inode, err := r.readInode(i)
	if err != nil {
		return nil, err
	}
	//log.Printf("i %d, inode: %T, %+v", i, inode, inode)
	switch x := inode.(type) {
	case dirInodeHeader:
		return &FileInfo{
			name:    name,
			size:    int64(x.FileSize),
			mode:    os.ModeDir | os.FileMode(x.Mode),
			modTime: time.Unix(int64(x.Mtime), 0),
			Inode:   i,
		}, nil

	case ldirInodeHeader:
		return &FileInfo{
			name:    name,
			size:    int64(x.FileSize),
			mode:    os.ModeDir | os.FileMode(x.Mode),
			modTime: time.Unix(int64(x.Mtime), 0),
			Inode:   i,
		}, nil

	case regInodeHeader:
		mode := os.FileMode(x.Mode & 0777)
		if x.Mode&syscall.S_ISUID != 0 {
			mode |= os.ModeSetuid
		}
		return &FileInfo{
			name:    name,
			size:    int64(x.FileSize),
			mode:    mode,
			modTime: time.Unix(int64(x.Mtime), 0),
			Inode:   i,
		}, nil

	case symlinkInodeHeader:
		return &FileInfo{
			name:    name,
			size:    int64(x.SymlinkSize),
			mode:    os.ModeSymlink | os.FileMode(x.Mode),
			modTime: time.Unix(int64(x.Mtime), 0),
			Inode:   i,
		}, nil
	}

	return nil, fmt.Errorf("unknown inode type %T", inode)
}

func (r *Reader) ReadLink(i Inode) (string, error) {
	// TODO: reduce code duplication with readInode
	blockoffset, offset := r.inode(i)
	br, err := r.blockReader(r.super.InodeTableStart+blockoffset, offset)
	if err != nil {
		return "", err
	}

	// We need the inode type before we know which type to pass to binary.Read,
	// so we need to read it twice:
	var inodeType uint16
	typeBuf := bytes.NewBuffer(make([]byte, 0, binary.Size(inodeType)))
	if err := binary.Read(io.TeeReader(br, typeBuf), binary.LittleEndian, &inodeType); err != nil {
		return "", err
	}
	br = io.MultiReader(typeBuf, br)

	if inodeType != symlinkType {
		return "", fmt.Errorf("invalid inode type: got %d instead of symlink", inodeType)
	}
	var si symlinkInodeHeader
	if err := binary.Read(br, binary.LittleEndian, &si); err != nil {
		return "", err
	}

	// Assumption: r.r is positioned right after the inode
	buf := make([]byte, si.SymlinkSize)
	if _, err := br.Read(buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func (r *Reader) FileReader(inode Inode) (*io.SectionReader, error) {
	//log.Printf("Readfile(%v)", inode)
	i, err := r.readInode(inode)
	if err != nil {
		return nil, err
	}
	ri := i.(regInodeHeader)
	//log.Printf("i: %+v", i)
	// TODO(compression): read the blocksizes to read compressed blocks
	off := int64(ri.StartBlock) + int64(ri.Offset)
	return io.NewSectionReader(r.r, off, int64(ri.FileSize)), nil
}

func (r *Reader) Readdir(dirInode Inode) ([]os.FileInfo, error) {
	//log.Printf("Readdir(%v (%x))", dirInode, dirInode)
	i, err := r.readInode(dirInode)
	if err != nil {
		return nil, err
	}
	var (
		startBlock int64
		fileSize   int64
		offset     int64
	)
	switch x := i.(type) {
	case dirInodeHeader:
		startBlock = int64(x.StartBlock)
		fileSize = int64(x.FileSize)
		offset = int64(x.Offset)

	case ldirInodeHeader:
		startBlock = int64(x.StartBlock)
		fileSize = int64(x.FileSize)
		offset = int64(x.Offset)

	default:
		return nil, fmt.Errorf("unknown directory inode type %T", i)
	}

	br, err := r.blockReader(r.super.DirectoryTableStart+startBlock, offset)
	if err != nil {
		return nil, err
	}

	// See also https://elixir.bootlin.com/linux/v4.18.9/source/fs/squashfs/dir.c#L63
	limit := fileSize - int64(len(".")) - int64(len(".."))
	br = io.LimitReader(br, limit)

	var fis []os.FileInfo
	for {
		var dh dirHeader
		if err := binary.Read(br, binary.LittleEndian, &dh); err != nil {
			if err == io.EOF {
				return fis, nil
			}
			return nil, err
		}
		dh.Count++ // SquashFS stores count-1
		//log.Printf("dh: %+v", dh)

		for i := 0; i < int(dh.Count); i++ {
			var de dirEntry
			if err := binary.Read(br, binary.LittleEndian, &de); err != nil {
				return nil, err
			}
			de.Size++ // SquashFS stores size-1
			//log.Printf("de: %+v", de)
			name := make([]byte, de.Size)
			if _, err := io.ReadFull(br, name); err != nil {
				return nil, err
			}
			//log.Printf("name: %q", string(name))

			fi, err := r.Stat(string(name), Inode(int64(dh.StartBlock)<<16|int64(de.Offset)))
			if err != nil {
				return nil, err
			}
			fis = append(fis, fi)
		}
	}

	return fis, nil
}

type FileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	Inode   Inode
}

func (fi *FileInfo) Name() string       { return fi.name }
func (fi *FileInfo) Size() int64        { return fi.size }
func (fi *FileInfo) Mode() os.FileMode  { return fi.mode }
func (fi *FileInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi *FileInfo) ModTime() time.Time { return fi.modTime }
func (fi *FileInfo) Sys() interface{}   { return fi }
