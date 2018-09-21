package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"

	"github.com/stapelberg/zi/internal/squashfs"
)

// wellKnown lists paths which should be created as a union overlay underneath
// /ro. E.g., /ro/bin will contain symlinks to all package’s bin directories, or
// /ro/system will contain symlinks to all package’s
// buildoutput/lib/systemd/system directories.
var wellKnown = []string{
	"bin",
	"buildoutput/lib/systemd/system",
	"buildoutput/lib/pkgconfig",
	// TODO: lib -> buildoutput/lib
	// TODO: usr/include -> buildoutput/include (or just include?)
}

func mountfuse(args []string) error {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	fset := flag.NewFlagSet("fuse", flag.ExitOnError)
	var (
		imgDir = fset.String("imgdir", filepath.Join(os.Getenv("HOME"), "zi/build/zi/pkg/"), "TODO")
	)
	fset.Parse(args)
	if fset.NArg() != 1 {
		return fmt.Errorf("syntax: fuse <mountpoint>")
	}
	mountpoint := fset.Arg(0)
	//log.Printf("mounting FUSE file system at %q", mountpoint)

	// TODO: use inotify to efficiently get updates to the store
	fis, err := ioutil.ReadDir(*imgDir)
	if err != nil {
		return err
	}

	farms := make(map[string]*farm)
	for _, wk := range wellKnown {
		// map from path underneath wk to target (symlinks)
		farms[wk] = &farm{
			byName: make(map[string]*symlink),
		}
	}
	var pkgs []string
	for _, fi := range fis {
		if !strings.HasSuffix(fi.Name(), ".squashfs") {
			continue
		}
		pkg := strings.TrimSuffix(fi.Name(), ".squashfs")
		pkgs = append(pkgs, pkg)

		// TODO: list contents using pure Go
		unsquashfs := exec.Command("unsquashfs", "-l", filepath.Join(*imgDir, fi.Name()))
		unsquashfs.Stderr = os.Stderr
		out, err := unsquashfs.Output()
		if err != nil {
			return fmt.Errorf("%v: %v", unsquashfs.Args, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			line = strings.TrimPrefix(line, "squashfs-root/")
			farm, ok := farms[filepath.Dir(line)]
			if !ok {
				continue
			}
			name := filepath.Base(line)
			if _, ok := farm.byName[name]; ok {
				//log.Printf("CONFLICT: %s claimed by 2 or more packages", name)
				continue
			}
			link := &symlink{
				name:   name,
				target: filepath.Join("..", pkg, line),
			}
			farm.links = append(farm.links, link)
			farm.byName[name] = link
		}
	}
	//log.Printf("farm: ")
	// for _, link := range farms["bin"].links {
	// 	log.Printf("  %s -> %s", link.name, link.target)
	// }

	server := fuseutil.NewFileSystemServer(&fuseFS{
		imgDir:  *imgDir,
		pkgs:    pkgs,
		readers: make([]*squashfs.Reader, len(pkgs)),
		farms:   farms,
	})

	mfs, err := fuse.Mount(mountpoint, server, &fuse.MountConfig{
		FSName:   "distri",
		ReadOnly: true,
		Options: map[string]string{
			"allow_other": "", // allow all users to read files
		},
	})
	if err != nil {
		return err
	}
	return mfs.Join(context.Background())
}

type symlink struct {
	name   string
	target string
}

type farm struct {
	// links is only ever appended to (nil entries are tombstones), because inodes.
	links []*symlink

	byName map[string]*symlink
}

// TODO: does fuseFS need a mutex? is there concurrency in FUSE at all?
type fuseFS struct {
	fuseutil.NotImplementedFileSystem

	imgDir string

	// pkgs is only ever appended to (empty strings are tombstones), because the
	// inode for /<pkg> is an index into pkgs.
	pkgs []string

	readers []*squashfs.Reader

	farms map[string]*farm
}

func (fs *fuseFS) mountImage(image int) error {
	//log.Printf("mountImage(%d)", image)
	if fs.readers[image] != nil {
		return nil // already mounted
	}
	f, err := os.Open(filepath.Join(fs.imgDir, fs.pkgs[image]+".squashfs"))
	if err != nil {
		return err
	}
	fs.readers[image], err = squashfs.NewReader(f)
	return err
}

func (fs *fuseFS) squashfsInode(i fuseops.InodeID) (int, squashfs.Inode, error) {
	// encoding scheme: <imagenr(uint16)> <startblock(uint32)> <offset(uint16)>
	// where imagenr starts at 1 (because 0 is an invalid inode in FUSE, but valid in SquashFS)
	image := int((i>>48)&0xFFFF) - 1
	i &= 0xFFFFFFFFFFFF // remove imagenr
	// We must support RootInodeID == 1: https://github.com/libfuse/libfuse/issues/267
	if i == fuseops.RootInodeID {
		if image == -1 {
			return image, 1, nil
		}
		if err := fs.mountImage(image); err != nil {
			return 0, 0, err
		}
		return image, fs.readers[image].RootInode(), nil
	}

	return image, squashfs.Inode(i), nil
}

func (fs *fuseFS) fuseInode(image int, i squashfs.Inode) fuseops.InodeID {
	//log.Printf("fuseInode(%d, %d) = %d", image, i, fuseops.InodeID(uint16(image+1))<<48|fuseops.InodeID(i))
	return fuseops.InodeID(uint16(image+1))<<48 | fuseops.InodeID(i)
}

func (fs *fuseFS) farmInode(i squashfs.Inode) (wkidx int, linkidx int) {
	return int(i >> 32), int(i & 0xFFFFFFFF)
}

func (fs *fuseFS) fuseAttributes(fi os.FileInfo) fuseops.InodeAttributes {
	return fuseops.InodeAttributes{
		Size:  uint64(fi.Size()),
		Nlink: 1, // TODO: number of incoming hard links to this inode
		Mode:  fi.Mode(),
		Atime: fi.ModTime(),
		Mtime: fi.ModTime(),
		Ctime: fi.ModTime(),
	}
}

func (fs *fuseFS) StatFS(ctx context.Context, op *fuseops.StatFSOp) error {
	//log.Printf("StatFS(op=%+v)", op)
	return fuse.ENOSYS
}

func (fs *fuseFS) LookUpInode(ctx context.Context, op *fuseops.LookUpInodeOp) error {
	//log.Printf("LookUpInode(op=%+v)", op)
	// find dirent op.Name in inode op.Parent
	image, squashfsInode, err := fs.squashfsInode(op.Parent)
	if err != nil {
		log.Println(err)
		return fuse.EIO
	}

	if image == -1 { // (virtual) root directory
		wkidx, linkidx := fs.farmInode(squashfsInode)
		//log.Printf("wkidx=%d, linkidx=%d", wkidx, linkidx)
		if wkidx == 0 && linkidx == 1 { // root directory (e.g. /ro)
			for idx, pkg := range fs.pkgs {
				if pkg != op.Name {
					continue
				}
				op.Entry.Child = fs.fuseInode(idx, 1 /* root */)
				op.Entry.Attributes = fuseops.InodeAttributes{
					Nlink: 1, // TODO: number of incoming hard links to this inode
					Mode:  os.ModeDir | 0555,
					Atime: time.Now(), // TODO
					Mtime: time.Now(), // TODO
					Ctime: time.Now(), // TODO
				}
				return nil
			}
			for idx, wk := range wellKnown {
				if filepath.Base(wk) != op.Name {
					continue
				}
				op.Entry.Child = fs.fuseInode(-1, squashfs.Inode(2+idx))
				op.Entry.Attributes = fuseops.InodeAttributes{
					Nlink: 1, // TODO: number of incoming hard links to this inode
					Mode:  os.ModeDir | 0555,
					Atime: time.Now(), // TODO
					Mtime: time.Now(), // TODO
					Ctime: time.Now(), // TODO
				}
				return nil
			}
			return fuse.ENOENT
		} else if wkidx == 0 { // farm root directory (e.g. /ro/bin)
			farm := fs.farms[wellKnown[linkidx-2]]
			for lidx, link := range farm.links {
				if link == nil {
					continue // tombstone
				}
				if link.name != op.Name {
					continue
				}

				op.Entry.Child = fs.fuseInode(-1, squashfs.Inode(linkidx)<<32|squashfs.Inode(lidx))
				op.Entry.Attributes = fuseops.InodeAttributes{
					Nlink: 1, // TODO: number of incoming hard links to this inode
					Mode:  os.ModeSymlink | 0444,
					Atime: time.Now(), // TODO
					Mtime: time.Now(), // TODO
					Ctime: time.Now(), // TODO
				}
				return nil
			}
			return fuse.ENOENT
		}
		//log.Printf("return EIO")
		return fuse.EIO
	}

	fis, err := fs.readers[image].Readdir(squashfsInode)
	if err != nil {
		//log.Printf("Readdir: %v", err)
		return fuse.EIO // TODO: what happens if we pass err?
	}

	for _, fi := range fis {
		if fi.Name() != op.Name {
			continue
		}
		op.Entry.Child = fs.fuseInode(image, fi.Sys().(*squashfs.FileInfo).Inode)
		op.Entry.Attributes = fs.fuseAttributes(fi)
		// TODO: fill in caching times
		return nil
	}

	return fuse.ENOENT
}

func (fs *fuseFS) GetInodeAttributes(ctx context.Context, op *fuseops.GetInodeAttributesOp) error {
	//log.Printf("GetInodeAttributes(op=%#v)", op)
	if op.Inode&0xFFFFFFFFFFFF == 1 {
		// prevent mounting of images for accessing the root (which happens when doing “ls /ro”)
		op.Attributes = fuseops.InodeAttributes{
			Nlink: 1, // TODO: number of incoming hard links to this inode
			Mode:  os.ModeDir | 0555,
			Atime: time.Now(), // TODO
			Mtime: time.Now(), // TODO
			Ctime: time.Now(), // TODO
		}
		return nil
	}
	image, squashfsInode, err := fs.squashfsInode(op.Inode)
	if err != nil {
		log.Println(err)
		return fuse.EIO
	}

	if image == -1 {
		wkidx, _ := fs.farmInode(squashfsInode)
		//log.Printf("wkidx %d, linkidx %d", wkidx, linkidx)
		if wkidx == 0 { // root directory of a farm
			idx := int(squashfsInode - 2)
			//log.Printf("well-known idx %d", idx)
			if idx >= len(wellKnown) {
				return fuse.ENOENT
			}
			op.Attributes = fuseops.InodeAttributes{
				Nlink: 1, // TODO: number of incoming hard links to this inode
				Mode:  os.ModeDir | 0555,
				Atime: time.Now(), // TODO
				Mtime: time.Now(), // TODO
				Ctime: time.Now(), // TODO
			}
			return nil
		}
		//farm := fs.farms[wellKnown[wkidx-1]]
		//link := farm.links[linkidx]
		op.Attributes = fuseops.InodeAttributes{
			Nlink: 1, // TODO: number of incoming hard links to this inode
			Mode:  os.ModeSymlink | 0444,
			Atime: time.Now(), // TODO
			Mtime: time.Now(), // TODO
			Ctime: time.Now(), // TODO
		}
		//log.Printf("return nil")
		return nil
	}

	fi, err := fs.readers[image].Stat("", squashfsInode)
	if err != nil {
		//log.Printf("Stat: %v", err)
		return fuse.ENOENT // TODO
	}
	op.Attributes = fs.fuseAttributes(fi)
	return nil
}

func (fs *fuseFS) OpenDir(ctx context.Context, op *fuseops.OpenDirOp) error {
	_, _, err := fs.squashfsInode(op.Inode)
	if err != nil {
		log.Println(err)
		return fuse.EIO
	}

	//log.Printf("OpenDir(op=%+v, image %d, inode %d)", op, image, squashfsInode)
	// TODO: open reader
	return nil // allow opening any directory
}

/*
  /ro                       inode img=0 wk=0 1
  /ro/bin                   inode img=0 wk=0 2
  /ro/system                inode img=0 wk=0 3
  /ro/system/docker.service inode img=0 wk=2 link=0
  /ro/glibc-2.27            inode img=1 1
*/

func (fs *fuseFS) ReadDir(ctx context.Context, op *fuseops.ReadDirOp) error {
	// TODO: if this inode is not referring to a directory, return fuse.EIO

	image, squashfsInode, err := fs.squashfsInode(op.Inode)
	if err != nil {
		log.Println(err)
		return fuse.EIO
	}

	//log.Printf("ReadDir(inode %d (image %d, i %d), handle %d, offset %d)", op.Inode, image, squashfsInode, op.Handle, op.Offset) // skip op.Dst, which is large

	if image == -1 { // (virtual) root directory
		var entries []fuseutil.Dirent
		if squashfsInode == 1 {
			for idx, pkg := range fs.pkgs {
				if pkg == "" {
					continue // tombstone
				}
				entries = append(entries, fuseutil.Dirent{
					Offset: fuseops.DirOffset(len(entries) + 1), // (opaque) offset of the next entry
					Inode:  fs.fuseInode(idx, 1 /* root */),
					Name:   pkg,
					Type:   fuseutil.DT_Directory,
				})
			}
			for idx, wk := range wellKnown {
				entries = append(entries, fuseutil.Dirent{
					Offset: fuseops.DirOffset(len(entries) + 1), // (opaque) offset of the next entry
					Inode:  fs.fuseInode(-1, squashfs.Inode(2+idx)),
					Name:   filepath.Base(wk),
					Type:   fuseutil.DT_Directory,
				})
			}
		} else { // well-known union directory
			idx := int(squashfsInode - 2)
			if idx >= len(wellKnown) {
				return fuse.ENOENT
			}
			farm := fs.farms[wellKnown[idx]] // TODO: store farms not by name, but by index?
			for lidx, link := range farm.links {
				if link == nil {
					continue // tombstone
				}
				entries = append(entries, fuseutil.Dirent{
					Offset: fuseops.DirOffset(len(entries) + 1), // (opaque) offset of the next entry
					Inode:  fs.fuseInode(-1, squashfs.Inode(1+idx)<<32|squashfs.Inode(lidx)),
					Name:   link.name,
					Type:   fuseutil.DT_File,
				})
			}
		}
		if op.Offset > fuseops.DirOffset(len(entries)) {
			return fuse.EIO
		}

		for _, e := range entries[op.Offset:] {
			n := fuseutil.WriteDirent(op.Dst[op.BytesRead:], e)
			if n == 0 {
				break
			}
			op.BytesRead += n
		}

		return nil
	}

	fis, err := fs.readers[image].Readdir(squashfsInode)
	if err != nil {
		//log.Printf("Readdir: %v", err)
		return fuse.EIO // TODO: what happens if we pass err?
	}

	if op.Offset > fuseops.DirOffset(len(fis)) {
		return fuse.EIO
	}

	for idx, e := range fis[op.Offset:] {
		direntType := fuseutil.DT_File
		if e.IsDir() {
			direntType = fuseutil.DT_Directory
		}
		dirent := fuseutil.Dirent{
			Offset: op.Offset + fuseops.DirOffset(idx) + 1, // (opaque) offset of the next entry
			Inode:  fs.fuseInode(image, e.Sys().(*squashfs.FileInfo).Inode),
			Name:   e.Name(),
			Type:   direntType,
		}
		//log.Printf("  dirent: %+v", dirent)
		n := fuseutil.WriteDirent(op.Dst[op.BytesRead:], dirent)
		if n == 0 {
			break
		}
		op.BytesRead += n
	}

	return nil
}

func (fs *fuseFS) OpenFile(ctx context.Context, op *fuseops.OpenFileOp) error {
	//log.Printf("OpenFile(op=%+v)", op)
	return nil // allow opening any file
}

func (fs *fuseFS) ReadFile(ctx context.Context, op *fuseops.ReadFileOp) error {
	//log.Printf("ReadFile(inode %d, handle %d, offset %d)", op.Inode, op.Handle, op.Offset) // skip op.Dst, which is large

	image, squashfsInode, err := fs.squashfsInode(op.Inode)
	if err != nil {
		log.Println(err)
		return fuse.EIO
	}

	r, err := fs.readers[image].FileReader(squashfsInode)
	if err != nil {
		return err
	}
	op.BytesRead, err = r.ReadAt(op.Dst, op.Offset)
	if err == io.EOF {
		err = nil // FUSE does not want io.EOF
	}
	return err
}

func (fs *fuseFS) ReadSymlink(ctx context.Context, op *fuseops.ReadSymlinkOp) error {
	//log.Printf("ReadSymlink(inode %d)", op.Inode)

	image, squashfsInode, err := fs.squashfsInode(op.Inode)
	if err != nil {
		log.Println(err)
		return fuse.EIO
	}

	if image == -1 {
		wkidx, linkidx := fs.farmInode(squashfsInode)
		//log.Printf("wkidx=%d, linkidx=%d", wkidx, linkidx)
		if wkidx == 0 {
			return fuse.EIO // no symlinks in root
		}
		// TODO: bounds checks
		farm := fs.farms[wellKnown[wkidx-2]]
		link := farm.links[linkidx]
		op.Target = link.target
		return nil
	}

	target, err := fs.readers[image].ReadLink(squashfsInode)
	if err != nil {
		return err
	}
	op.Target = target
	return nil
}
