package fuse

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
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
	"google.golang.org/grpc"

	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/internal/squashfs"
	"github.com/distr1/distri/pb"
)

const Help = `TODO
`

// wellKnown lists paths which should be created as a union overlay underneath
// /ro. E.g., /ro/bin will contain symlinks to all package’s bin directories, or
// /ro/system will contain symlinks to all package’s
// out/lib/systemd/system directories.
var ExchangeDirs = []string{
	"/bin",
	"/out/lib",
	"/out/lib/gio",
	"/out/lib/girepository-1.0",
	"/out/include",
	"/out/share/aclocal",
	"/out/share/gettext",
	"/out/share/gir-1.0",
	"/out/share/glib-2.0/schemas",
	"/out/share/mime",
	"/out/share/man",
	"/out/share/dbus-1",
	"/out/share/fonts/truetype",
	"/out/share/X11/xorg.conf.d",
	"/out/gopath",
}

const (
	rootInode = 1
	ctlInode  = 2
)

type FileNotFoundError struct {
	path string
}

func (e *FileNotFoundError) Error() string {
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
	return 0, &FileNotFoundError{path: component}
}

func LookupPath(rd *squashfs.Reader, path string) (squashfs.Inode, error) {
	inode := rd.RootInode()
	parts := strings.Split(path, "/")
	for idx, part := range parts {
		var err error
		inode, err = lookupComponent(rd, inode, part)
		if err != nil {
			if _, ok := err.(*FileNotFoundError); ok {
				return 0, &FileNotFoundError{path: path}
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
			return LookupPath(rd, target)
		}
	}
	return inode, nil
}

func Mount(args []string) (join func(context.Context) error, _ error) {
	//log.SetFlags(log.LstdFlags | log.Lshortfile)
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
		for _, dir := range ExchangeDirs {
			if !permitted[dir] {
				continue
			}
			filtered = append(filtered, dir)
		}
		ExchangeDirs = filtered
	}

	// TODO: use inotify to efficiently get updates to the store

	fs := &fuseFS{
		repo:        *repo,
		fileReaders: make(map[fuseops.InodeID]*io.SectionReader),
		inodeCnt:    2, // root + ctl inode
		dirs:        make(map[string]*dir),
		inodes:      make(map[fuseops.InodeID]interface{}),
		unions:      make(map[fuseops.InodeID][]fuseops.InodeID),
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
	fs.growReaders(len(pkgs))

	if err := fs.scanPackagesLocked(pkgs); err != nil {
		return nil, err
	}

	var libRequested bool
	for _, dir := range ExchangeDirs {
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
		fs.symlink(fs.dirs["/lib"], "../glibc-amd64-2.27/out/lib/ld-linux-x86-64.so.2")
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
			"suid":        "",
		},
		//DebugLogger: log.New(os.Stderr, "[debug] ", log.LstdFlags),
	})
	if err != nil {
		return nil, err
	}
	join = mfs.Join

	{
		tempdir, err := ioutil.TempDir("", "distri-fuse")
		if err != nil {
			return nil, err
		}
		join = func(ctx context.Context) error {
			defer os.RemoveAll(tempdir)
			return mfs.Join(ctx)
		}
		fs.ctl = filepath.Join(tempdir, "distri-fuse-ctl")
		ln, err := net.Listen("unix", fs.ctl)
		if err != nil {
			return nil, err
		}
		srv := grpc.NewServer()
		pb.RegisterFUSEServer(srv, fs)
		go func() {
			if err := srv.Serve(ln); err != nil {
				log.Fatal(err)
			}
		}()
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

	return join, nil
}

// dirent is a directory entry.
// * ReadDir returns name, inode and typ()
// * LookUpInode returns inode and mode()
// * GetInodeAttributes returns mode()
// * ReadSymlink returns linkTarget
type dirent struct {
	name       string // e.g. "xterm"
	linkTarget string // e.g. "../../xterm-amd64-23/bin/xterm". Empty for directories
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
	dircache   map[squashfs.Inode]map[string]fuseops.ChildInodeEntry
}

type fuseFS struct {
	fuseutil.NotImplementedFileSystem

	repo string
	ctl  string

	mu       sync.Mutex
	inodeCnt fuseops.InodeID
	dirs     map[string]*dir
	inodes   map[fuseops.InodeID]interface{} // *dirent (file) or *dir (dir)
	unions   map[fuseops.InodeID][]fuseops.InodeID
	// pkgs is only ever appended to (empty strings are tombstones), because the
	// inode for /<pkg> is an index into pkgs.
	pkgs []string
	// readers contains one SquashFS reader for every package, or nil if the
	// package has not yet been accessed.
	readers []*squashfsReader

	fileReadersMu sync.Mutex
	fileReaders   map[fuseops.InodeID]*io.SectionReader
}

func (fs *fuseFS) growReaders(n int) {
	// Increase capacity to hold as many readers as we now have packages:
	readers := make([]*squashfsReader, n)
	copy(readers, fs.readers)
	fs.readers = readers
}

func (fs *fuseFS) reader(image int) *squashfsReader {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.readers[image]
}

func (fs *fuseFS) union(inode fuseops.InodeID) []fuseops.InodeID {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.unions[inode]
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
	start := time.Now()
	defer func() {
		log.Printf("scanPackages in %v", time.Since(start))
	}()
	// TODO: iterate over packages once, calling mkdir for all exchange dirs
	for _, dir := range ExchangeDirs {
		fs.mkExchangeDirAll(strings.TrimPrefix(dir, "/out"))
	}

	existing := make(map[string]bool)
	for _, pkg := range fs.pkgs {
		existing[pkg] = true
	}

	for idx, pkg := range pkgs {
		if existing[pkg] {
			continue
		}
		fs.pkgs = append(fs.pkgs, pkg)
		f, err := os.Open(filepath.Join(fs.repo, pkg+".squashfs"))
		if err != nil {
			return err
		}
		// TODO: close or reuse f
		rd, err := squashfs.NewReader(f)
		if err != nil {
			return err
		}

		// set up runtime_unions:
		meta, err := pb.ReadMetaFile(filepath.Join(fs.repo, pkg+".meta.textproto"))
		if err != nil {
			return err
		}
		for _, o := range meta.GetRuntimeUnion() {
			// log.Printf("%s: runtime union: %v", pkg, o)
			image := -1
			for idx, pkg := range fs.pkgs {
				if pkg != o.GetPkg() {
					continue
				}
				image = idx
				break
			}
			if image == -1 {
				log.Printf("%s: runtime union: pkg %q not found", pkg, o.GetPkg())
				continue // o.pkg not found
			}

			dstinode, err := LookupPath(rd, "out/"+o.GetDir())
			if err != nil {
				if _, ok := err.(*FileNotFoundError); ok {
					continue // nothing to overlay, skip this package
				}
				return err
			}

			if err := fs.mountImage(image); err != nil {
				return err
			}
			rd := fs.readers[image]
			srcinode, err := LookupPath(rd.Reader, "out/"+o.GetDir())
			if err != nil {
				if _, ok := err.(*FileNotFoundError); ok {
					log.Printf("%s: runtime union: %s/out/%s not found", pkg, o.GetPkg(), o.GetDir())
					continue
				}
				return err
			}

			srcfuse := fs.fuseInode(image, srcinode)
			dstfuse := fs.fuseInode(idx, dstinode)
			fs.unions[srcfuse] = append(fs.unions[srcfuse], dstfuse)
			delete(rd.dircache, srcinode) // invalidate dircache
		}

		type pathWithInode struct {
			path  string
			inode squashfs.Inode
		}
		inodes := make([]pathWithInode, 0, len(ExchangeDirs))
		for _, path := range ExchangeDirs {
			inode, err := LookupPath(rd, strings.TrimPrefix(path, "/"))
			if err != nil {
				if _, ok := err.(*FileNotFoundError); ok {
					continue
				}
				return err
			}
			inodes = append(inodes, pathWithInode{path, inode})
		}

		for len(inodes) > 0 {
			path, inode := inodes[0].path, inodes[0].inode
			inodes = inodes[1:]
			exchangePath := strings.TrimPrefix(path, "/out")
			dir, ok := fs.dirs[exchangePath]
			if !ok {
				panic(fmt.Sprintf("BUG: fs.dirs[%q] not found", exchangePath))
			}
			sfis, err := rd.Readdir(inode)
			if err != nil {
				return fmt.Errorf("Readdir(%s, %s): %v", pkg, dir, err)
			}
			for _, sfi := range sfis {
				if sfi.Mode().IsDir() {
					dir := filepath.Join(path, sfi.Name())
					fs.mkExchangeDirAll(strings.TrimPrefix(dir, "/out"))
					inodes = append(inodes, pathWithInode{dir, sfi.Sys().(*squashfs.FileInfo).Inode})
					continue
				}
				rel, err := filepath.Rel(exchangePath, filepath.Join("/", pkg, path, sfi.Name()))
				if err != nil {
					return err
				}
				fs.symlink(dir, rel)
			}
		}
	}

	fs.growReaders(len(fs.pkgs))

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

	existing := make(map[string]bool)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, pkg := range fs.pkgs {
		existing[pkg] = true
	}
	for _, pkg := range mm.GetPackage() {
		if existing[pkg.GetName()] {
			continue
		}
		fs.pkgs = append(fs.pkgs, pkg.GetName())
		for _, p := range pkg.GetWellKnownPath() {
			exchangePath := strings.TrimPrefix(filepath.Dir(p), "out")
			dir, ok := fs.dirs[exchangePath]
			if !ok {
				panic(fmt.Sprintf("BUG: fs.dirs[%q] not found", exchangePath))
			}
			rel, err := filepath.Rel(exchangePath, filepath.Join("/", pkg.GetName(), p))
			if err != nil {
				return err
			}
			fs.symlink(dir, rel)
		}
	}

	fs.growReaders(len(fs.pkgs))

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
		return err
		// if !os.IsNotExist(err) {
		// 	return err
		// }
		// f, err = updateAndOpen("/tmp/imgdir" /*fs.repo*/, remote+"/"+pkg+".squashfs")
		// if err != nil {
		// 	return err
		// }
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
		dircache: make(map[squashfs.Inode]map[string]fuseops.ChildInodeEntry),
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
				op.Entry.Child = fs.fuseInode(idx, rootInode)
				op.Entry.Attributes = fuseops.InodeAttributes{
					Nlink: 1, // TODO: number of incoming hard links to this inode
					Mode:  os.ModeDir | 0555,
					Atime: time.Now(), // TODO
					Mtime: time.Now(), // TODO
					Ctime: time.Now(), // TODO
				}
				return nil
			}
			if op.Name == "ctl" {
				op.Entry.Child = ctlInode
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
		fis = make(map[string]fuseops.ChildInodeEntry)
		ur := fs.newUnionReader(op.Parent)
		for ur.Next() {
			image := ur.Image()
			for _, fi := range ur.Dir() {
				fis[fi.Name()] = fuseops.ChildInodeEntry{
					Child:      fs.fuseInode(image, fi.Sys().(*squashfs.FileInfo).Inode),
					Attributes: fs.fuseAttributes(fi),
				}
			}
		}
		if err := ur.Err(); err != nil {
			log.Printf("Readdir: %v", err)
			return fuse.EIO
		}
		// It is okay if another goroutine races us to getting this lock: the
		// contents will be the same, and an extra write doesn’t hurt.
		rd.dircacheMu.Lock()
		rd.dircache[squashfsInode] = fis
		rd.dircacheMu.Unlock()
	}

	cie, ok := fis[op.Name]
	if !ok {
		return fuse.ENOENT
	}
	op.Entry.Child = cie.Child
	op.Entry.Attributes = cie.Attributes
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
					Inode:  fs.fuseInode(idx, rootInode),
					Name:   pkg,
					Type:   fuseutil.DT_Directory,
				})
			}

			entries = append(entries, fuseutil.Dirent{
				Offset: fuseops.DirOffset(len(entries) + 1), // (opaque) offset of the next entry
				Inode:  fs.fuseInode(-1, ctlInode),
				Name:   "ctl",
				Type:   fuseutil.DT_File,
			})

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

	var fis []fuseutil.Dirent
	ur := fs.newUnionReader(op.Inode)
	for ur.Next() {
		image := ur.Image()
		for _, e := range ur.Dir() {
			direntType := fuseutil.DT_File
			if e.IsDir() {
				direntType = fuseutil.DT_Directory
			}
			fis = append(fis, fuseutil.Dirent{
				Offset: fuseops.DirOffset(len(fis)) + 1, // (opaque) offset of the next entry
				Inode:  fs.fuseInode(image, e.Sys().(*squashfs.FileInfo).Inode),
				Name:   e.Name(),
				Type:   direntType,
			})
		}
	}
	if err := ur.Err(); err != nil {
		log.Printf("Readdir: %v", err)
		return fuse.EIO
	}

	if op.Offset > fuseops.DirOffset(len(fis)) {
		return fuse.EIO
	}

	for _, dirent := range fis[op.Offset:] {
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
		if op.Inode == ctlInode {
			op.Target = fs.ctl
			return nil
		}
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

func (fs *fuseFS) Ping(ctx context.Context, req *pb.PingRequest) (*pb.PingReply, error) {
	return &pb.PingReply{}, nil
}

func (fs *fuseFS) MkdirAll(ctx context.Context, req *pb.MkdirAllRequest) (*pb.MkdirAllReply, error) {
	if req.GetDir() == "" {
		return nil, fmt.Errorf("MkdirAll: dir must not be empty")
	}
	if strings.Contains(req.GetDir(), "/") {
		return nil, fmt.Errorf("MkdirAll: dir must not contain slashes")
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, pkg := range fs.pkgs {
		if pkg == req.GetDir() {
			return &pb.MkdirAllReply{}, nil
		}
	}
	fs.mkExchangeDirAll("/" + req.GetDir())
	return &pb.MkdirAllReply{}, nil
}

func (fs *fuseFS) ScanPackages(ctx context.Context, req *pb.ScanPackagesRequest) (*pb.ScanPackagesReply, error) {
	pkgs, err := fs.findPackages()
	if err != nil {
		return nil, err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return &pb.ScanPackagesReply{}, fs.scanPackagesLocked(pkgs)
}
