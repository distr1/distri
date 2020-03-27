package fuse

import (
	"context"
	"errors"
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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"
	"golang.org/x/sync/errgroup"
	"golang.org/x/xerrors"
	"google.golang.org/grpc"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/internal/squashfs"
	"github.com/distr1/distri/pb"
)

const help = `distri fuse [-flags] <mountpoint>

Mount the distri FUSE file system.

Example:
  % distri fuse /ro
`

// wellKnown lists paths which should be created as a union overlay underneath
// /ro. E.g., /ro/bin will contain symlinks to all package’s bin directories, or
// /ro/system will contain symlinks to all package’s
// out/lib/systemd/system directories.
var ExchangeDirs = []string{
	"/bin",
	"/out/lib",
	"/out/lib64",
	"/out/lib/gio",
	"/out/lib/girepository-1.0",
	"/out/include",
	"/out/share",
	"/out/share/aclocal",
	"/out/share/gettext",
	"/out/share/gir-1.0",
	"/out/share/glib-2.0/schemas",
	"/out/share/mime",
	"/out/gopath",
	"/debug",
}

// TODO: pprof label for each of the exchange dirs so that we can profile them

const (
	rootInode = 1
	ctlInode  = 2
)

func Mount(ctx context.Context, args []string) (join func(context.Context) error, _ error) {
	//log.SetFlags(log.LstdFlags | log.Lshortfile)
	fset := flag.NewFlagSet("fuse", flag.ExitOnError)
	var (
		repo         = fset.String("repo", env.DefaultRepo, "TODO")
		readiness    = fset.Int("readiness", -1, "file descriptor on which to send readiness notification")
		overlays     = fset.String("overlays", "", "comma-separated list of overlays to provide. if empty, all overlays will be provided")
		pkgsList     = fset.String("pkgs", "", "comma-separated list of packages to provide. if empty, all packages within -repo will be provided")
		autoDownload = fset.Bool("autodownload", false, "simulate availability of all packages, automatically downloading them as required. works well for e.g. /ro-dbg")
		section      = fset.String("section", "pkg", "repository section to serve (one of pkg, debug, src)")
	)
	fset.Usage = func() {
		fmt.Fprintln(os.Stderr, help)
		fmt.Fprintf(os.Stderr, "Flags for distri %s:\n", fset.Name())
		fset.PrintDefaults()
	}
	fset.Parse(args)
	if fset.NArg() != 1 {
		return nil, xerrors.Errorf("syntax: fuse <mountpoint>")
	}
	mountpoint := fset.Arg(0)
	//log.Printf("mounting FUSE file system at %q", mountpoint)

	remotes, err := env.Repos()
	if err != nil {
		return nil, err
	}
	if len(remotes) == 0 {
		remotes = []distri.Repo{{Path: *repo}}
	}

	// TODO: do what fusermount -u does, i.e. umount2("/ro-dbg", UMOUNT_NOFOLLOW)

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
		repo:         *repo,
		remoteRepos:  remotes,
		autoDownload: *autoDownload,
		repoSection:  *section,
		fileReaders:  make(map[fuseops.InodeID]*io.SectionReader),
		inodeCnt:     2, // root + ctl inode
		dirs:         make(map[string]*dir),
		inodes:       make(map[fuseops.InodeID]interface{}),
		unions:       make(map[fuseops.InodeID][]fuseops.InodeID),
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

	if err := fs.scanPackages(&nopLocker{}, pkgs); err != nil {
		return nil, err
	}

	if fs.autoDownload {
		if err := fs.updatePackages(); err != nil {
			log.Printf("updatePackages: %v", err)
			// Retry in the background every 10 seconds until we succeed
			go func() {
				for range time.Tick(10 * time.Second) {
					if err := fs.updatePackages(); err != nil {
						log.Printf("updatePackages: %v", err)
						continue
					}
					break
				}
			}()
		}
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
		fs.mkExchangeDirAll(&nopLocker{}, "/lib")
		fs.symlink(fs.dirs["/lib"], "../glibc-amd64-2.27-1/out/lib/ld-linux-x86-64.so.2")
	}

	server := fuseutil.NewFileSystemServer(fs)

	go func() {
		if !fs.autoDownload {
			return
		}
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
				err = fs.scanPackages(&nopLocker{}, pkgs)
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
		// Opt into caching resolved symlinks in the kernel page cache:
		EnableSymlinkCaching: true,
		// Opt into returning -ENOSYS on OpenFile and OpenDir:
		EnableNoOpenSupport:    true,
		EnableNoOpendirSupport: true,
		//DebugLogger: log.New(os.Stderr, "[debug] ", log.LstdFlags),
	})
	if err != nil {
		return nil, xerrors.Errorf("fuse.Mount: %v", err)
	}
	join = func(ctx context.Context) error {
		defer syscall.Unmount(mountpoint, 0)
		return mfs.Join(ctx)
	}

	{
		tempdir, err := ioutil.TempDir("", "distri-fuse")
		if err != nil {
			return nil, err
		}
		join = func(ctx context.Context) error {
			defer func() {
				if err := fuse.Unmount(mountpoint); err != nil {
					fmt.Fprintf(os.Stderr, "fuse.Unmount: %v\n", err)
				}
			}()
			defer func() {
				if err := os.RemoveAll(tempdir); err != nil {
					fmt.Fprintf(os.Stderr, "cleanup: %v\n", err)
				}
			}()
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

func (d *dirent) size() uint64 {
	if d.linkTarget != "" {
		return uint64(len(d.linkTarget))
	}
	return 0
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

	repo         string
	remoteRepos  []distri.Repo
	ctl          string
	autoDownload bool
	repoSection  string // e.g. “debug” (default “pkg”)

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

func (fs *fuseFS) mkExchangeDirAll(mu sync.Locker, path string) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := fs.dirs[path]; exists {
		return // fast path
	}
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
	current := dir.byName[base]
	if current != nil {
		if current.linkTarget == "" {
			return // do not shadow exchange directories
		}
		versionTarget := distri.ParseVersion(target)
		versionCurrent := distri.ParseVersion(current.linkTarget)
		if versionTarget.Pkg != versionCurrent.Pkg {
			return // different package already owns this link
		}
		if versionTarget.DistriRevision < versionCurrent.DistriRevision {
			return // more recent link target already in place
		}
		for idx, entry := range dir.entries {
			if entry == nil || entry.name != base {
				continue
			}
			dir.entries[idx] = nil // tombstone
		}
	}
	dirent := &dirent{
		name:       base,
		linkTarget: target,
		inode:      fs.allocateInodeLocked(),
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
	pkgs := make([]string, 0, len(fis))
	for _, fi := range fis {
		if !strings.HasSuffix(fi.Name(), ".squashfs") {
			continue
		}
		pkg := strings.TrimSuffix(fi.Name(), ".squashfs")
		pkgs = append(pkgs, pkg)
	}
	// Need to sort again: removing the .squashfs suffix reverses sort order of
	// e.g. [less-amd64-530-2.squashfs less-amd64-530.squashfs]
	// to   [less-amd64-530.squashfs less-amd64-530-2.squashfs]
	// ('.' > '-' because 0x2E > 0x2D)
	sort.Slice(pkgs, func(i, j int) bool {
		return distri.PackageRevisionLess(pkgs[i], pkgs[j])
	})
	return pkgs, nil
}

func countSlashes(filename string) int {
	var offset int
	count := 1
	idx := strings.IndexByte(filename, '/')
	for idx != -1 {
		count++
		offset += idx + 1
		idx = strings.IndexByte(filename[offset:], '/')
	}
	return count
}

func (fs *fuseFS) scanPackagesSymlink(mu sync.Locker, rd *squashfs.Reader, pkg string, exchangeDirs []string) error {
	type pathWithInode struct {
		path  string
		inode squashfs.Inode
	}
	inodes := make([]pathWithInode, 0, len(exchangeDirs))
	for _, path := range exchangeDirs {
		inode, err := rd.LookupPath(strings.TrimPrefix(path, "/"))
		if err != nil {
			if _, ok := err.(*squashfs.FileNotFoundError); ok {
				continue
			}
			return err
		}
		inodes = append(inodes, pathWithInode{filepath.Clean(path), inode})
	}

	for len(inodes) > 0 {
		path, inode := inodes[0].path, inodes[0].inode
		inodes = inodes[1:]
		exchangePath := strings.TrimPrefix(path, "/out")
		prefix := strings.Repeat("../", countSlashes(exchangePath)-1)
		mu.Lock()
		dir, ok := fs.dirs[exchangePath]
		mu.Unlock()
		if !ok {
			panic(fmt.Sprintf("BUG: fs.dirs[%q] not found", exchangePath))
		}
		sfis, err := rd.ReaddirNoStat(inode)
		if err != nil {
			return xerrors.Errorf("Readdir(%s, %v): %v", pkg, dir, err)
		}
		for _, sfi := range sfis {
			if sfi.Mode().IsDir() {
				dir := filepath.Join(path, sfi.Name())
				fs.mkExchangeDirAll(mu, strings.TrimPrefix(dir, "/out"))
				inodes = append(inodes, pathWithInode{dir, sfi.Sys().(*squashfs.FileInfo).Inode})
				continue
			}
			full := "/" + pkg + path + "/" + sfi.Name()
			rel := prefix + full[1:]
			mu.Lock()
			fs.symlink(dir, rel)
			mu.Unlock()
		}
	}
	return nil
}

var errSkipPackage = errors.New("sentinel: skip package")

func (fs *fuseFS) scanPackage(mu sync.Locker, idx int, pkg string) error {
	f, err := os.Open(filepath.Join(fs.repo, pkg+".squashfs"))
	if err != nil {
		return err
	}
	defer f.Close()
	rd, err := squashfs.NewReader(f)
	if err != nil {
		return err
	}

	// set up runtime_unions:
	meta, err := pb.ReadMetaFile(filepath.Join(fs.repo, pkg+".meta.textproto"))
	if err != nil {
		if os.IsNotExist(err) {
			if fs.repoSection != "debug" {
				log.Print(err)
				return errSkipPackage // recover by skipping this package
			}
		} else {
			return err
		}
	}
	for _, o := range meta.GetRuntimeUnion() {
		// log.Printf("%s: runtime union: %v", pkg, o)
		image := -1
		mu.Lock()
		for idx, pkg := range fs.pkgs {
			if pkg != o.GetPkg() {
				continue
			}
			image = idx
			break
		}
		mu.Unlock()
		if image == -1 {
			log.Printf("%s: runtime union: pkg %q not found", pkg, o.GetPkg())
			continue // o.pkg not found
		}

		dstinode, err := rd.LookupPath("out/" + o.GetDir())
		if err != nil {
			if _, ok := err.(*squashfs.FileNotFoundError); ok {
				continue // nothing to overlay, skip this package
			}
			return err
		}

		err = fs.mountImage(image)
		mu.Lock()
		rd := fs.readers[image]
		mu.Unlock()
		if err != nil {
			return err
		}

		srcinode, err := rd.Reader.LookupPath("out/" + o.GetDir())
		if err != nil {
			if _, ok := err.(*squashfs.FileNotFoundError); ok {
				log.Printf("%s: runtime union: %s/out/%s not found", pkg, o.GetPkg(), o.GetDir())
				continue
			}
			return err
		}

		mu.Lock()
		srcfuse := fs.fuseInode(image, srcinode)
		dstfuse := fs.fuseInode(idx, dstinode)
		fs.unions[srcfuse] = append(fs.unions[srcfuse], dstfuse)
		mu.Unlock()
		delete(rd.dircache, srcinode) // invalidate dircache
	}

	if err := fs.scanPackagesSymlink(mu, rd, pkg, ExchangeDirs); err != nil {
		return err
	}

	return nil
}

func (fs *fuseFS) scanPackages(mu sync.Locker, pkgs []string) error {
	start := time.Now()
	defer func() {
		log.Printf("scanPackages in %v", time.Since(start))
	}()
	// TODO: iterate over packages once, calling mkdir for all exchange dirs
	for _, dir := range ExchangeDirs {
		fs.mkExchangeDirAll(mu, strings.TrimPrefix(dir, "/out"))
	}

	existing := make(map[string]bool)
	for _, pkg := range fs.pkgs {
		existing[pkg] = true
	}

	{
		mu := mu // shadow, possibly overwrite:
		if _, ok := mu.(*nopLocker); ok {
			mu = &sync.Mutex{} // ensure locking during errgroup
		}

		var eg errgroup.Group
		for idx, pkg := range pkgs {
			if existing[pkg] {
				delete(existing, pkg) // left-overs are deleted packages
				continue
			}
			idx, pkg := idx, pkg // copy
			eg.Go(func() error {
				if err := fs.scanPackage(mu, idx, pkg); err != nil {
					if err == errSkipPackage {
						return nil
					}
					log.Println(err)
					// Not loading a package is a better failure mode than
					// e.g. distri fuse (which is required for early system
					// boot) not starting anymore.
					return nil
				}
				mu.Lock()
				fs.pkgs = append(fs.pkgs, pkg)
				mu.Unlock()
				return nil
			})
		}
		if err := eg.Wait(); err != nil {
			return err
		}
	}

	if leftover := existing; len(leftover) > 0 {
		// iterate through all symlinks, checking if they begin with /ro/<pkg>,
		// and <pkg> matching any of the still-present ones
		scan := make(map[string]bool)
		for path, dir := range fs.dirs {
			for idx, dirent := range dir.entries {
				if dirent == nil {
					continue // tombstone
				}
				if dirent.linkTarget == "" {
					continue // subdirectory
				}
				// e.g. /lib/pkgconfig/bash.pc → ../../bash-amd64-1/out/lib/pkgconfig/bash.pc
				target := filepath.Clean(filepath.Join(filepath.Dir(path), dirent.linkTarget))
				// target is now /bash-amd64-1/out/lib/pkgconfig/bash.pc
				pkg := target[1 : 1+strings.IndexByte(target[1:], '/')]
				if leftover[pkg] {
					scan[path] = true

					// delete, in case there is no stand-in (or the stand-in
					// does not contain the file)
					delete(dir.byName, dirent.name)
					dir.entries[idx] = nil // tombstone
				}
			}
		}
		affectedExchangeDirs := make([]string, 0, len(scan))
		for path := range scan {
			affectedExchangeDirs = append(affectedExchangeDirs, path)
		}
		for deleted := range existing {
			var standin string
			for _, arch := range []string{"amd64", "i686"} {
				archmiddle := "-" + arch + "-"
				if !strings.Contains(deleted, archmiddle) {
					continue
				}
				source := deleted[:strings.Index(deleted, archmiddle)+len(archmiddle)]
				matches, err := filepath.Glob(filepath.Join(fs.repo, source+"*.squashfs"))
				if err != nil {
					return err
				}
				if len(matches) == 0 {
					continue
				}
				for idx, m := range matches {
					matches[idx] = strings.TrimSuffix(filepath.Base(m), ".squashfs")
				}
				sort.Slice(matches, func(i, j int) bool {
					return distri.PackageRevisionLess(matches[j], matches[i]) // reverse
				})
				standin = matches[0]
				break
			}
			if standin == "" {
				continue
			}
			f, err := os.Open(filepath.Join(fs.repo, standin+".squashfs"))
			if err != nil {
				return err
			}
			defer f.Close()
			rd, err := squashfs.NewReader(f)
			if err != nil {
				return err
			}

			if err := fs.scanPackagesSymlink(mu, rd, standin, affectedExchangeDirs); err != nil {
				return err
			}
		}
	}

	fs.growReaders(len(fs.pkgs))

	return nil
}

type nopLocker struct{}

func (*nopLocker) Lock()   {}
func (*nopLocker) Unlock() {}

func (fs *fuseFS) updatePackages() error {
	// TODO: make this code work with multiple repos
	resp, err := http.Get(fs.remoteRepos[0].Path + "/" + fs.repoSection + "/meta.binaryproto")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		return xerrors.Errorf("HTTP status %v", resp.Status)
	}
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return xerrors.Errorf("reading meta.binaryproto: %v", err)
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
			exchangePath := "/" + strings.TrimPrefix(filepath.Dir(p), "out/")
			fs.mkExchangeDirAll(&nopLocker{}, exchangePath)
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
		if !os.IsNotExist(err) {
			return err
		}
		if !fs.autoDownload {
			return err
		}
		f, err = autodownload(fs.repo, fs.remoteRepos[0].Path, fs.repoSection+"/"+pkg+".squashfs")
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

// VirtualFileExpiration determines how long virtual files (e.g. exchange
// directory contents) are cached. 1s matches the default entry_timeout FUSE
// option. Larger values (e.g. never) have no effect.
const VirtualFileExpiration = 1 * time.Second

func (fs *fuseFS) LookUpInode(ctx context.Context, op *fuseops.LookUpInodeOp) error {
	//log.Printf("LookUpInode(op=%+v)", op)
	// find dirent op.Name in inode op.Parent
	image, squashfsInode, err := fs.squashfsInode(op.Parent)
	if err != nil {
		log.Println(err)
		return fuse.EIO
	}

	if image == -1 { // (virtual) root directory

		// Cache virtual files for 1s, which is the default entry_timeout FUSE
		// option value. Enabling caching speeds up building the i3 package from
		// 46s to 18s. Larger values (e.g. never) have no effect.
		op.Entry.AttributesExpiration = time.Now().Add(VirtualFileExpiration)
		op.Entry.EntryExpiration = time.Now().Add(VirtualFileExpiration)

		if squashfsInode == 1 { // root directory (e.g. /ro)
			fs.mu.Lock()
			defer fs.mu.Unlock()
			for _, dirent := range fs.dirs["/"].entries {
				if dirent.name != op.Name {
					continue
				}
				op.Entry.Child = dirent.inode
				op.Entry.Attributes = fuseops.InodeAttributes{
					Size:  dirent.size(),
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
					Size:  uint64(len(fs.ctl)),
					Nlink: 1, // TODO: number of incoming hard links to this inode
					Mode:  os.ModeSymlink | 0444,
					Atime: time.Now(), // TODO
					Mtime: time.Now(), // TODO
					Ctime: time.Now(), // TODO
				}
				return nil
			}
			// TODO: switch to returning nil once we invalidate the cache
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
				return nil // same as ENOENT when op.Entry.Child is 0
			}
			op.Entry.Child = dirent.inode
			op.Entry.Attributes = fuseops.InodeAttributes{
				Size:  dirent.size(),
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

	op.Entry.AttributesExpiration = never
	op.Entry.EntryExpiration = never

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
		return nil // same as ENOENT when op.Entry.Child is 0
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
		if op.Inode == ctlInode {
			op.Attributes = fuseops.InodeAttributes{
				Size:  uint64(len(fs.ctl)),
				Nlink: 1, // TODO: number of incoming hard links to this inode
				Mode:  os.ModeSymlink | 0444,
				Atime: time.Now(), // TODO
				Mtime: time.Now(), // TODO
				Ctime: time.Now(), // TODO
			}
			return nil
		}

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
		case *dirent:
			op.Attributes = fuseops.InodeAttributes{
				Size:  x.size(),
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
	// Instruct the kernel to not send OpenDir requests for performance:
	// https://github.com/torvalds/linux/commit/7678ac50615d9c7a491d9861e020e4f5f71b594c
	return fuse.ENOSYS
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
	// Instruct the kernel to not send OpenFile requests for performance:
	// https://github.com/torvalds/linux/commit/7678ac50615d9c7a491d9861e020e4f5f71b594c
	return fuse.ENOSYS
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

func (fs *fuseFS) ListXattr(ctx context.Context, op *fuseops.ListXattrOp) error {
	image, squashfsInode, err := fs.squashfsInode(op.Inode)
	if err != nil {
		log.Println(err)
		return fuse.EIO
	}
	if image == -1 {
		return nil // no extended attributes
	}

	attrs, err := fs.reader(image).ReadXattrs(squashfsInode)
	if err != nil {
		return err
	}
	for _, attr := range attrs {
		op.BytesRead += len(attr.FullName) + 1 /* NUL-terminated */
	}
	if op.BytesRead > len(op.Dst) {
		if len(op.Dst) == 0 {
			return nil
		}
		return syscall.ERANGE
	}
	copied := 0
	for _, attr := range attrs {
		copy(op.Dst[copied:], []byte(attr.FullName))
		copied += len(attr.FullName) + 1 /* NUL-terminated */
		op.Dst[copied-1] = 0
	}
	return nil
}

func (fs *fuseFS) GetXattr(ctx context.Context, op *fuseops.GetXattrOp) error {
	image, squashfsInode, err := fs.squashfsInode(op.Inode)
	if err != nil {
		log.Println(err)
		return fuse.EIO
	}
	if image == -1 {
		if len(op.Dst) > 0 {
			op.Dst[0] = 0
			op.BytesRead = 1
		}
		return nil // no extended attributes
	}

	attrs, err := fs.reader(image).ReadXattrs(squashfsInode)
	if err != nil {
		return err
	}
	var val []byte
	for _, attr := range attrs {
		if attr.FullName != op.Name {
			continue
		}
		val = attr.Value
		break
	}
	if val == nil {
		return syscall.ENODATA
	}
	op.BytesRead = len(val)
	if op.BytesRead > len(op.Dst) {
		if len(op.Dst) == 0 {
			return nil
		}
		return syscall.ERANGE
	}
	copy(op.Dst, val)
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
		return nil, xerrors.Errorf("MkdirAll: dir must not be empty")
	}
	if strings.Contains(req.GetDir(), "/") {
		return nil, xerrors.Errorf("MkdirAll: dir must not contain slashes")
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, pkg := range fs.pkgs {
		if pkg == req.GetDir() {
			return &pb.MkdirAllReply{}, nil
		}
	}
	fs.mkExchangeDirAll(&nopLocker{}, "/"+req.GetDir())
	return &pb.MkdirAllReply{}, nil
}

func (fs *fuseFS) ScanPackages(ctx context.Context, req *pb.ScanPackagesRequest) (*pb.ScanPackagesReply, error) {
	pkgs, err := fs.findPackages()
	if err != nil {
		return nil, err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return &pb.ScanPackagesReply{}, fs.scanPackages(&nopLocker{}, pkgs)
}
