package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"

	"github.com/stapelberg/zi/internal/env"
	"github.com/stapelberg/zi/internal/squashfs"
	"github.com/stapelberg/zi/pb"
)

const fuseHelp = `TODO
`

// wellKnown lists paths which should be created as a union overlay underneath
// /ro. E.g., /ro/bin will contain symlinks to all package’s bin directories, or
// /ro/system will contain symlinks to all package’s
// buildoutput/lib/systemd/system directories.
var exchangeDirs = []string{
	"/bin",
	"/buildoutput/lib",
	"/buildoutput/lib/systemd/system",
	"/buildoutput/lib/sysusers.d",
	"/buildoutput/lib/tmpfiles.d",
	"/buildoutput/lib/pkgconfig",
	"/buildoutput/lib/xorg/modules",
	"/buildoutput/lib/xorg/modules/drivers",
	"/buildoutput/include",
	"/buildoutput/include/sys",  // libcap and glibc
	"/buildoutput/include/scsi", // linux-4.18.7 and glibc-2.27
	"/buildoutput/include/X11",
	"/buildoutput/share/man/man1",
	"/buildoutput/share/dbus-1/system.d",
	"/buildoutput/share/dbus-1/system-services",
	"/buildoutput/share/dbus-1/services",
}

type fileNotFoundError struct {
	path string
}

func (e *fileNotFoundError) Error() string {
	return fmt.Sprintf("%q not found", e.path)
}

func lookupComponent(rd *squashfs.Reader, parent squashfs.Inode, component string) (squashfs.Inode, error) {
	rfis, err := rd.Readdir(parent)
	if err != nil {
		return 0, err
	}
	for _, rfi := range rfis {
		if rfi.Name() == component {
			return rfi.Sys().(*squashfs.FileInfo).Inode, nil
		}
	}
	return 0, &fileNotFoundError{path: component}
}

func lookupPath(rd *squashfs.Reader, path string) (squashfs.Inode, error) {
	inode := rd.RootInode()
	parts := strings.Split(path, "/")
	for idx, part := range parts {
		var err error
		inode, err = lookupComponent(rd, inode, part)
		if err != nil {
			if _, ok := err.(*fileNotFoundError); ok {
				return 0, &fileNotFoundError{path: path}
			}
			return 0, err
		}
		fi, err := rd.Stat("", inode)
		if err != nil {
			return 0, fmt.Errorf("Stat(%d): %v", inode, err)
		}
		if fi.Mode()&os.ModeSymlink > 0 {
			target, err := rd.ReadLink(inode)
			if err != nil {
				return 0, err
			}
			//log.Printf("component %q (full: %q) resolved to %q", part, parts[:idx+1], target)
			target = filepath.Clean(filepath.Join(append(parts[:idx] /* parent */, target)...))
			//log.Printf("-> %s", target)
			return lookupPath(rd, target)
		}
	}
	return inode, nil
}

func mountfuse(args []string) (join func(context.Context) error, _ error) {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	fset := flag.NewFlagSet("fuse", flag.ExitOnError)
	var (
		repo      = fset.String("repo", env.DefaultRepo, "TODO")
		readiness = fset.Int("readiness", -1, "file descriptor on which to send readiness notification")
		overlays  = fset.String("overlays", "", "comma-separated list of overlays to provide. if empty, all overlays will be provided")
		pkgsList  = fset.String("pkgs", "", "comma-separated list of packages to provide. if empty, all packages within -repo will be provided")
	)
	fset.Parse(args)
	if fset.NArg() != 1 {
		return nil, fmt.Errorf("syntax: fuse <mountpoint>")
	}
	mountpoint := fset.Arg(0)
	//log.Printf("mounting FUSE file system at %q", mountpoint)

	if *overlays != "" {
		var filtered []string
		permitted := make(map[string]bool)
		for _, overlay := range strings.Split(strings.TrimSpace(*overlays), ",") {
			if overlay == "" {
				continue
			}
			permitted[overlay] = true
		}
		for _, dir := range exchangeDirs {
			if !permitted[dir] {
				continue
			}
			filtered = append(filtered, dir)
		}
		exchangeDirs = filtered
	}

	// TODO: use inotify to efficiently get updates to the store

	fs := &fuseFS{
		repo:        *repo,
		fileReaders: make(map[fuseops.InodeID]*io.SectionReader),
		inodeCnt:    1, // root inode
		dirs:        make(map[string]*dir),
		inodes:      make(map[fuseops.InodeID]interface{}),
	}
	dir := &dir{
		byName: make(map[string]*dirent),
	}
	fs.dirs["/"] = dir
	fs.inodes[fs.inodeCnt] = dir

	var pkgs []string
	if *pkgsList != "" {
		pkgs = strings.Split(strings.TrimSpace(*pkgsList), ",")
	} else {
		var err error
		pkgs, err = fs.findPackages()
		if err != nil {
			return nil, err
		}
	}

	if err := fs.scanPackagesLocked(pkgs); err != nil {
		return nil, err
	}

	var libRequested bool
	for _, dir := range exchangeDirs {
		if dir == "/lib" {
			libRequested = true
			break
		}
	}
	if !libRequested {
		// Even if the /lib exchange dir was not requested, we still need to
		// provide a symlink to ld-linux.so, which is used as the .interp of our
		// ELF binaries.
		fs.mkExchangeDirAll("/lib")
		fs.symlink(fs.dirs["/lib"], "../glibc-2.27/buildoutput/lib/ld-linux-x86-64.so.2")
	}

	server := fuseutil.NewFileSystemServer(fs)

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGHUP)
		for range c {
			log.Printf("updating packages upon SIGHUP")
			if err := fs.updatePackages(); err != nil {
				log.Printf("updatePackages: %v", err)
			}
		}
	}()

	// Set up signal handler for rescanning the repo, but only if the package
	// list is not filtered:
	if *pkgsList == "" {
		go func() {
			c := make(chan os.Signal, 1)
			signal.Notify(c, syscall.SIGUSR1)
			for range c {
				log.Printf("scanning packages upon SIGUSR1")
				pkgs, err := fs.findPackages()
				if err != nil {
					log.Printf("findPackages: %v", err)
					continue
				}
				fs.mu.Lock()
				err = fs.scanPackagesLocked(pkgs)
				fs.mu.Unlock()
				if err != nil {
					log.Printf("scanPackages: %v", err)
				}
				log.Printf("scan done")
			}
		}()
	}

	// logf, err := os.Create("/tmp/fuse.log")
	// if err != nil {
	// 	return nil, err
	// }
	mfs, err := fuse.Mount(mountpoint, server, &fuse.MountConfig{
		FSName:   "distri",
		ReadOnly: true,
		Options: map[string]string{
			"allow_other": "", // allow all users to read files
		},
		//DebugLogger: log.New(logf, "[debug] ", log.LstdFlags),
	})
	if err != nil {
		return nil, err
	}
	if *readiness != -1 {
		os.NewFile(uintptr(*readiness), "").Close()
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	go func() {
		<-ch
		syscall.Unmount(mountpoint, 0)
		// The following os.Exit is typically unreached because the above
		// unmount causes mfs.Join to return.
		os.Exit(128 + int(syscall.SIGINT))
	}()

	return mfs.Join, nil
}

// dirent is a directory entry.
// * ReadDir returns name, inode and typ()
// * LookUpInode returns inode and mode()
// * GetInodeAttributes returns mode()
// * ReadSymlink returns linkTarget
type dirent struct {
	name       string // e.g. "xterm"
	linkTarget string // e.g. "../../xterm-23/bin/xterm". Empty for directories
	inode      fuseops.InodeID
}

func (d *dirent) typ() fuseutil.DirentType {
	if d.linkTarget != "" {
		return fuseutil.DT_File
	}
	return fuseutil.DT_Directory
}

func (d *dirent) mode() os.FileMode {
	if d.linkTarget != "" {
		return os.ModeSymlink | 0444
	}
	return os.ModeDir | 0555
}

type dir struct {
	entries []*dirent          // ReadDir requires deterministic iteration order
	byName  map[string]*dirent // LookUpInode profits from fast access by name
}

type squashfsReader struct {
	*squashfs.Reader

	file *os.File // for closing it in Destroy

	dircacheMu sync.Mutex
	dircache   map[squashfs.Inode]map[string]os.FileInfo
}

type fuseFS struct {
	fuseutil.NotImplementedFileSystem

	repo string

	mu       sync.Mutex
	inodeCnt fuseops.InodeID
	dirs     map[string]*dir
	inodes   map[fuseops.InodeID]interface{} // *dirent (file) or *dir (dir)
	// pkgs is only ever appended to (empty strings are tombstones), because the
	// inode for /<pkg> is an index into pkgs.
	pkgs []string
	// readers contains one SquashFS reader for every package, or nil if the
	// package has not yet been accessed.
	readers []*squashfsReader

	fileReadersMu sync.Mutex
	fileReaders   map[fuseops.InodeID]*io.SectionReader
}

func (fs *fuseFS) reader(image int) *squashfsReader {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.readers[image]
}

func (fs *fuseFS) allocateInodeLocked() fuseops.InodeID {
	fs.inodeCnt++
	return fs.inodeCnt
}

func (fs *fuseFS) mkExchangeDirAll(path string) {
	components := strings.Split(path, "/")
	for idx, component := range components[1:] {
		path := strings.Join(components[:idx+2], "/")
		if fs.dirs[path] != nil {
			continue
		}
		dir := &dir{
			byName: make(map[string]*dirent),
		}
		fs.dirs[path] = dir
		parentPath := filepath.Clean("/" + strings.Join(components[:idx+1], "/"))
		parent, ok := fs.dirs[parentPath]
		if !ok {
			panic(fmt.Sprintf("BUG: %q not found", parentPath))
		}
		dirent := &dirent{
			name:  component,
			inode: fs.allocateInodeLocked(),
		}
		parent.entries = append(parent.entries, dirent)
		parent.byName[dirent.name] = dirent // might shadow an old symlink dirent
		fs.inodes[dirent.inode] = dir
	}
}

func (fs *fuseFS) symlink(dir *dir, target string) {
	base := filepath.Base(target)
	dirent := &dirent{
		name:       base,
		linkTarget: target,
		inode:      fs.allocateInodeLocked(),
	}
	for idx, entry := range dir.entries {
		if entry == nil || entry.name != base {
			continue
		}
		if entry.linkTarget == "" {
			return // do not shadow exchange directories
		}
		dir.entries[idx] = nil // tombstone
		break
	}
	dir.entries = append(dir.entries, dirent)
	dir.byName[base] = dirent
	fs.inodes[dirent.inode] = dirent
}

func (fs *fuseFS) findPackages() ([]string, error) {
	fis, err := ioutil.ReadDir(fs.repo)
	if err != nil {
		return nil, err
	}

	var pkgs []string
	for _, fi := range fis {
		if !strings.HasSuffix(fi.Name(), ".squashfs") {
			continue
		}
		pkg := strings.TrimSuffix(fi.Name(), ".squashfs")
		pkgs = append(pkgs, pkg)
	}
	return pkgs, nil
}

func (fs *fuseFS) scanPackagesLocked(pkgs []string) error {
	// TODO: iterate over packages once, calling mkdir for all exchange dirs
	for _, dir := range exchangeDirs {
		fs.mkExchangeDirAll(strings.TrimPrefix(dir, "/buildoutput"))
	}

	existing := make(map[string]bool)
	for _, pkg := range fs.pkgs {
		existing[pkg] = true
	}

	for _, pkg := range pkgs {
		if existing[pkg] {
			continue
		}
		fs.pkgs = append(fs.pkgs, pkg)
		f, err := os.Open(filepath.Join(fs.repo, pkg+".squashfs"))
		if err != nil {
			return err
		}
		rd, err := squashfs.NewReader(f)
		if err != nil {
			return err
		}
		for _, path := range exchangeDirs {
			exchangePath := strings.TrimPrefix(path, "/buildoutput")
			dir, ok := fs.dirs[exchangePath]
			if !ok {
				panic(fmt.Sprintf("BUG: fs.dirs[%q] not found", exchangePath))
			}
			inode, err := lookupPath(rd, strings.TrimPrefix(path, "/"))
			if err != nil {
				if _, ok := err.(*fileNotFoundError); ok {
					continue
				}
				return err
			}
			sfis, err := rd.Readdir(inode)
			if err != nil {
				return fmt.Errorf("Readdir(%s, %s): %v", pkg, dir, err)
			}
			for _, sfi := range sfis {
				rel, err := filepath.Rel(exchangePath, filepath.Join("/", pkg, path, sfi.Name()))
				if err != nil {
					return err
				}
				fs.symlink(dir, rel)
			}
		}
	}

	// Increase capacity to hold as many readers as we now have packages:
	readers := make([]*squashfsReader, len(fs.pkgs))
	copy(readers, fs.readers)
	fs.readers = readers

	return nil
}

// TODO: read this from a config file, remove trailing slash if any (always added by caller)
const remote = "http://kwalitaet:alpha@midna.zekjur.net:8045/export"

func (fs *fuseFS) updatePackages() error {
	resp, err := http.Get(remote + "/meta.binaryproto")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		return fmt.Errorf("HTTP status %v", resp.Status)
	}
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading meta.binaryproto: %v", err)
	}
	var mm pb.MirrorMeta
	if err := proto.Unmarshal(b, &mm); err != nil {
		return err
	}
	log.Printf("%d remote packages", len(mm.GetPackage()))

	// pkgs := make(map[string]bool)
	// fs.mu.Lock()
	// defer fs.mu.Unlock()
	// for _, pkg := range fs.pkgs {
	// 	pkgs[pkg] = true
	// }
	// for _, pkg := range mm.GetPackage() {
	// 	if pkgs[pkg.GetName()] {
	// 		continue
	// 	}
	// 	fs.pkgs = append(fs.pkgs, pkg.GetName())
	// 	for _, p := range pkg.GetWellKnownPath() {
	// 		dir := filepath.Dir(p)
	// 		name := filepath.Base(p)
	// 		farm, ok := fs.farms[dir]
	// 		if !ok {
	// 			continue
	// 		}
	// 		if _, ok := farm.byName[name]; ok {
	// 			//log.Printf("CONFLICT: %s claimed by 2 or more packages", name)
	// 			continue
	// 		}
	// 		link := &symlink{
	// 			name:   name,
	// 			target: filepath.Join("..", pkg.GetName(), dir, name),
	// 			idx:    len(farm.links),
	// 		}
	// 		farm.links = append(farm.links, link)
	// 		farm.byName[name] = link
	// 	}
	// }
	// // Increase capacity to hold as many readers as we now have packages:
	// readers := make([]*squashfsReader, len(fs.pkgs))
	// copy(readers, fs.readers)
	// fs.readers = readers

	return nil
}

func (fs *fuseFS) mountImage(image int) error {
	//log.Printf("mountImage(%d)", image)
	if fs.reader(image) != nil {
		return nil // already mounted
	}

	fs.mu.Lock()
	pkg := fs.pkgs[image]
	fs.mu.Unlock()
	log.Printf("mounting %s", pkg)

	// var err error
	// f := &httpReaderAt{fileurl: "http://localhost:7080/" + pkg + ".squashfs"}
	f, err := os.Open(filepath.Join(fs.repo, pkg+".squashfs"))
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		f, err = updateAndOpen("/tmp/imgdir" /*fs.repo*/, remote+"/"+pkg+".squashfs")
		if err != nil {
			return err
		}
	}
	rd, err := squashfs.NewReader(f)
	if err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.readers[image] = &squashfsReader{
		file:     f,
		Reader:   rd,
		dircache: make(map[squashfs.Inode]map[string]os.FileInfo),
	}
	return nil
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
		return image, fs.reader(image).RootInode(), nil
	}

	return image, squashfs.Inode(i), nil
}

func (fs *fuseFS) fuseInode(image int, i squashfs.Inode) fuseops.InodeID {
	//log.Printf("fuseInode(%d, %d) = %d", image, i, fuseops.InodeID(uint16(image+1))<<48|fuseops.InodeID(i))
	return fuseops.InodeID(uint16(image+1))<<48 | fuseops.InodeID(i)
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
	op.BlockSize = 4096
	op.Blocks = 1 // TODO: sum up package size once we need it
	op.BlocksFree = 0
	op.BlocksAvailable = 0
	op.IoSize = 65536 // preferred size of reads
	return nil
}

// never is used for FUSE expiration timestamps. Since the package store is
// immutable and inodes are stable, the kernel can cache all values forever.
//
// The value is named never even though, strictly speaking, it refers to one
// year in the future, because we can take a cache miss once every year and
// there is no sentinel value meaning never in FUSE.
var never = time.Now().Add(365 * 24 * time.Hour)

func (fs *fuseFS) LookUpInode(ctx context.Context, op *fuseops.LookUpInodeOp) error {
	op.Entry.AttributesExpiration = never
	op.Entry.EntryExpiration = never
	//log.Printf("LookUpInode(op=%+v)", op)
	// find dirent op.Name in inode op.Parent
	image, squashfsInode, err := fs.squashfsInode(op.Parent)
	if err != nil {
		log.Println(err)
		return fuse.EIO
	}

	if image == -1 { // (virtual) root directory
		if squashfsInode == 1 { // root directory (e.g. /ro)
			fs.mu.Lock()
			defer fs.mu.Unlock()
			for _, dirent := range fs.dirs["/"].entries {
				if dirent.name != op.Name {
					continue
				}
				op.Entry.Child = dirent.inode
				op.Entry.Attributes = fuseops.InodeAttributes{
					Nlink: 1, // TODO: number of incoming hard links to this inode
					Mode:  dirent.mode(),
					Atime: time.Now(), // TODO
					Mtime: time.Now(), // TODO
					Ctime: time.Now(), // TODO
				}
				return nil
			}
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
			return fuse.ENOENT
		} else { // overlay directory
			fs.mu.Lock()
			defer fs.mu.Unlock()
			dir, ok := fs.inodes[op.Parent].(*dir)
			if !ok {
				return fuse.EIO // not a directory
			}
			dirent, ok := dir.byName[op.Name]
			if !ok {
				return fuse.ENOENT
			}
			op.Entry.Child = dirent.inode
			op.Entry.Attributes = fuseops.InodeAttributes{
				Nlink: 1, // TODO: number of incoming hard links to this inode
				Mode:  dirent.mode(),
				Atime: time.Now(), // TODO
				Mtime: time.Now(), // TODO
				Ctime: time.Now(), // TODO
			}
			return nil
		}
		//log.Printf("return EIO")
		return fuse.EIO
	}

	rd := fs.reader(image)
	rd.dircacheMu.Lock()
	fis, ok := rd.dircache[squashfsInode]
	rd.dircacheMu.Unlock()
	if !ok {
		fis = make(map[string]os.FileInfo)
		dfis, err := rd.Readdir(squashfsInode)
		if err != nil {
			//log.Printf("Readdir: %v", err)
			return fuse.EIO // TODO: what happens if we pass err?
		}
		for _, fi := range dfis {
			fis[fi.Name()] = fi
		}
		// It is okay if another goroutine races us to getting this lock: the
		// contents will be the same, and an extra write doesn’t hurt.
		rd.dircacheMu.Lock()
		rd.dircache[squashfsInode] = fis
		rd.dircacheMu.Unlock()
	}

	fi, ok := fis[op.Name]
	if !ok {
		return fuse.ENOENT
	}
	op.Entry.Child = fs.fuseInode(image, fi.Sys().(*squashfs.FileInfo).Inode)
	op.Entry.Attributes = fs.fuseAttributes(fi)
	return nil
}

func (fs *fuseFS) GetInodeAttributes(ctx context.Context, op *fuseops.GetInodeAttributesOp) error {
	op.AttributesExpiration = never
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
		fs.mu.Lock()
		defer fs.mu.Unlock()
		x, ok := fs.inodes[op.Inode]
		if !ok {
			return fuse.ENOENT
		}
		switch x := x.(type) {
		case *dir:
			op.Attributes = fuseops.InodeAttributes{
				Nlink: 1, // TODO: number of incoming hard links to this inode
				Mode:  os.ModeDir | 0555,
				Atime: time.Now(), // TODO
				Mtime: time.Now(), // TODO
				Ctime: time.Now(), // TODO
			}
			return nil
		case *dirent:
			op.Attributes = fuseops.InodeAttributes{
				Nlink: 1, // TODO: number of incoming hard links to this inode
				Mode:  x.mode(),
				Atime: time.Now(), // TODO
				Mtime: time.Now(), // TODO
				Ctime: time.Now(), // TODO
			}
		}
		return nil
	}

	fi, err := fs.reader(image).Stat("", squashfsInode)
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
  /ro                       inode img=0 1
  /ro/bin                   inode img=0 2
  /ro/system                inode img=0 7
  /ro/system/docker.service inode img=0 8
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
			fs.mu.Lock()
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
			for _, dirent := range fs.dirs["/"].entries {
				entries = append(entries, fuseutil.Dirent{
					Offset: fuseops.DirOffset(len(entries) + 1), // (opaque) offset of the next entry
					Inode:  dirent.inode,
					Name:   dirent.name,
					Type:   fuseutil.DT_Directory,
				})
			}
			fs.mu.Unlock()
		} else { // exchange directory
			fs.mu.Lock()
			dir, ok := fs.inodes[op.Inode].(*dir)
			if !ok {
				return fuse.EIO
			}
			for _, dirent := range dir.entries {
				if dirent == nil {
					continue // tombstone
				}
				entries = append(entries, fuseutil.Dirent{
					Offset: fuseops.DirOffset(len(entries) + 1), // (opaque) offset of the next entry
					Inode:  dirent.inode,
					Name:   dirent.name,
					Type:   dirent.typ(),
				})
			}
			fs.mu.Unlock()
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

	fis, err := fs.reader(image).Readdir(squashfsInode)
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

	op.KeepPageCache = true // no modifications are happening in immutable images

	return nil // allow opening any file
}

func (fs *fuseFS) ReadFile(ctx context.Context, op *fuseops.ReadFileOp) error {
	//log.Printf("ReadFile(inode %d, handle %d, offset %d)", op.Inode, op.Handle, op.Offset) // skip op.Dst, which is large
	fs.fileReadersMu.Lock()
	r, ok := fs.fileReaders[op.Inode]
	fs.fileReadersMu.Unlock()
	if !ok {
		image, squashfsInode, err := fs.squashfsInode(op.Inode)
		if err != nil {
			log.Println(err)
			return fuse.EIO
		}

		r, err = fs.reader(image).FileReader(squashfsInode)
		if err != nil {
			return err
		}
		fs.fileReadersMu.Lock()
		fs.fileReaders[op.Inode] = r
		fs.fileReadersMu.Unlock()
	}
	var err error
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
		fs.mu.Lock()
		defer fs.mu.Unlock()
		dirent, ok := fs.inodes[op.Inode].(*dirent)
		if !ok {
			return fuse.EIO // not a symlink
		}
		op.Target = dirent.linkTarget
		return nil
	}

	target, err := fs.reader(image).ReadLink(squashfsInode)
	if err != nil {
		return err
	}
	op.Target = target
	return nil
}

func (fs *fuseFS) Destroy() {
	for _, rd := range fs.readers {
		if rd == nil {
			continue
		}
		rd.file.Close()
	}
}
