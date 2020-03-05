package fuse

import (
	"os"

	"github.com/distr1/distri/internal/squashfs"
	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
)

type unionReader struct {
	inodes []fuseops.InodeID
	fs     *fuseFS
	idx    int
	image  int
	dir    []os.FileInfo
	err    error
}

func (fs *fuseFS) newUnionReader(dir fuseops.InodeID) *unionReader {
	return &unionReader{
		inodes: append([]fuseops.InodeID{dir}, fs.union(dir)...),
		fs:     fs,
	}
}

func (mr *unionReader) Next() bool {
	if mr.idx > len(mr.inodes)-1 {
		return false
	}
	var squashfsInode squashfs.Inode
	mr.image, squashfsInode, mr.err = mr.fs.squashfsInode(mr.inodes[mr.idx])
	if mr.err != nil {
		mr.err = fuse.EIO
		return false
	}

	if mr.err = mr.fs.mountImage(mr.image); mr.err != nil {
		return false
	}
	mr.dir, mr.err = mr.fs.reader(mr.image).Readdir(squashfsInode)
	mr.idx++
	return mr.err == nil
}

func (mr *unionReader) Dir() []os.FileInfo { return mr.dir }
func (mr *unionReader) Err() error         { return mr.err }
func (mr *unionReader) Image() int         { return mr.image }
