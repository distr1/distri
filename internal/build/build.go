package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/dwarf"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/env"
	cmdfuse "github.com/distr1/distri/internal/fuse"
	"github.com/distr1/distri/internal/squashfs"
	"github.com/distr1/distri/internal/trace"
	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"
	"github.com/google/renameio"
	"github.com/jacobsa/fuse"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
	"golang.org/x/xerrors"
	"google.golang.org/grpc"
)

// TODO: central source of truth for these
const (
	tidBuildpkg = iota
	tidSquashfsSrc
)

// TODO: move cp machinery into separate package

func copyFile(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return nil
}

// stringsFromByteSlice converts a sequence of attributes to a []string.
// On Linux, each entry is a NULL-terminated string.
func stringsFromByteSlice(buf []byte) []string {
	var result []string
	off := 0
	for i, b := range buf {
		if b == 0 {
			result = append(result, string(buf[off:i]))
			off = i + 1
		}
	}
	return result
}

func readXattrs(fd int) ([]squashfs.Xattr, error) {
	sz, err := unix.Flistxattr(fd, nil)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, sz)
	sz, err = unix.Flistxattr(fd, buf)
	if err != nil {
		return nil, err
	}
	var attrs []squashfs.Xattr
	attrnames := stringsFromByteSlice(buf)
	for _, attr := range attrnames {
		sz, err := unix.Fgetxattr(fd, attr, nil)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, sz)
		sz, err = unix.Fgetxattr(fd, attr, buf)
		if err != nil {
			return nil, err
		}
		attrs = append(attrs, squashfs.XattrFromAttr(attr, buf))
	}
	return attrs, nil
}

type cpFileInfo struct {
	// config
	root string
	dir  string

	// contents
	fi       os.FileInfo
	children []*cpFileInfo
	byName   map[string]*cpFileInfo // children, keyed by fi.Name()
}

func cpscan(root, dir string) ([]*cpFileInfo, error) {
	var children []*cpFileInfo

	fis, err := ioutil.ReadDir(filepath.Join(root, dir))
	if err != nil {
		return nil, err
	}
	walk := func(root string, fis []os.FileInfo) error {
		for _, fi := range fis {
			info := &cpFileInfo{fi: fi, root: root, dir: dir}
			if fi.IsDir() {
				ch, err := cpscan(
					root,
					filepath.Join(dir, fi.Name()))
				if err != nil {
					return err
				}
				info.byName = make(map[string]*cpFileInfo)
				for _, child := range ch {
					info.byName[child.fi.Name()] = child
				}
				info.children = ch
			} else if fi.Mode().IsRegular() {
			} else if fi.Mode()&os.ModeSymlink != 0 {
			} else {
				log.Printf("ERROR: unsupported file: %v", filepath.Join(dir, fi.Name()))
				continue
			}
			children = append(children, info)
		}
		return nil
	}

	// root is e.g. /tmp/integrationbuild212641383/build/debug/source
	if err := walk(root, fis); err != nil {
		return nil, err
	}

	return children, nil
}

func cpExtraFiles(root string, ef map[string][]os.FileInfo) error {
	return nil
}

func (cpFi *cpFileInfo) mkdirAll(path, parent string) (*cpFileInfo, error) {
	first := path
	rest := ""
	if idx := strings.IndexByte(first, '/'); idx > -1 {
		first, rest = first[:idx], first[idx+1:]
	}
	// log.Printf("mkdirAll(path=%s, parent=%s) first=%s rest=%s", path, parent, first, rest)
	fi, ok := cpFi.byName[first]
	if !ok {
		info, err := os.Stat(filepath.Join(parent, first))
		if err != nil {
			return nil, err
		}

		fi = &cpFileInfo{
			fi:     info,
			root:   cpFi.root,
			dir:    cpFi.dir,
			byName: make(map[string]*cpFileInfo),
		}
		// log.Printf("adding child %v", fi)
		cpFi.addChild(fi)
	}
	if rest == "" {
		return fi, nil
	}
	return fi.mkdirAll(rest, filepath.Join(parent, first))
}

func (cpFi *cpFileInfo) lookup(path string) (*cpFileInfo, error) {
	first := path
	rest := ""
	if idx := strings.IndexByte(first, '/'); idx > -1 {
		first, rest = first[:idx], first[idx+1:]
	}
	// log.Printf("lookup(path=%s) first=%s rest=%s", path, first, rest)
	fi, ok := cpFi.byName[first]
	if !ok {
		return nil, fmt.Errorf("%s not found", first)
	}
	if rest == "" {
		return fi, nil
	}
	return fi.lookup(rest)
}

func (cpFi *cpFileInfo) addChild(child *cpFileInfo) {
	if _, ok := cpFi.byName[child.fi.Name()]; ok {
		return // already exists
	}
	cpFi.children = append(cpFi.children, child)
	cpFi.byName[child.fi.Name()] = child
}

func (cpFi *cpFileInfo) copyTo(w *squashfs.Directory) error {
	// for convenience:
	fi := cpFi.fi
	dir := filepath.Join(cpFi.root, cpFi.dir)

	// log.Printf("(%s/%s).copyTo(dir=%s)", cpFi.dir, cpFi.fi.Name(), dir)

	if fi.IsDir() {
		subdir := w.Directory(fi.Name(), fi.ModTime())
		for _, child := range cpFi.children {
			if err := child.copyTo(subdir); err != nil {
				return err
			}
		}
		subdir.Flush()
	} else if fi.Mode().IsRegular() {
		in, err := os.Open(filepath.Join(dir, fi.Name()))
		if err != nil {
			return err
		}
		defer in.Close()
		attrs, err := readXattrs(int(in.Fd()))
		if err != nil {
			return err
		}
		f, err := w.File(fi.Name(), fi.ModTime(), uint16(fi.Sys().(*syscall.Stat_t).Mode), attrs)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, in); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		in.Close()
	} else if fi.Mode()&os.ModeSymlink != 0 {
		// isSymlink
		dest, err := os.Readlink(filepath.Join(dir, fi.Name()))
		if err != nil {
			return err
		}
		if err := w.Symlink(dest, fi.Name(), fi.ModTime(), fi.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

func cp(w *squashfs.Directory, dir string) error {
	//log.Printf("cp(%s)", dir)
	fis, err := ioutil.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, fi := range fis {
		//log.Printf("file %s, mode %#o (raw %#o)", fi.Name(), fi.Mode(), fi.Sys().(*syscall.Stat_t).Mode)
		if fi.IsDir() {
			subdir := w.Directory(fi.Name(), fi.ModTime())
			if err := cp(subdir, filepath.Join(dir, fi.Name())); err != nil {
				return err
			}
		} else if fi.Mode().IsRegular() {
			in, err := os.Open(filepath.Join(dir, fi.Name()))
			if err != nil {
				return err
			}
			defer in.Close()
			attrs, err := readXattrs(int(in.Fd()))
			if err != nil {
				return err
			}
			f, err := w.File(fi.Name(), fi.ModTime(), uint16(fi.Sys().(*syscall.Stat_t).Mode), attrs)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, in); err != nil {
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			in.Close()
		} else if fi.Mode()&os.ModeSymlink != 0 {
			dest, err := os.Readlink(filepath.Join(dir, fi.Name()))
			if err != nil {
				return err
			}
			if err := w.Symlink(dest, fi.Name(), fi.ModTime(), fi.Mode().Perm()); err != nil {
				return err
			}
		} else {
			log.Printf("ERROR: unsupported file: %v", filepath.Join(dir, fi.Name()))
		}
	}
	return w.Flush()
}

// Ctx is a build context: it contains state about a build.
type Ctx struct {
	Proto     *pb.Build `json:"-"`
	PkgDir    string    // e.g. /home/michael/distri/pkgs/busybox
	Pkg       string    // e.g. busybox
	Arch      string    // e.g. amd64
	Version   string    // e.g. 1.29.2
	SourceDir string    // e.g. /home/michael/distri/build/busybox/busybox-1.29.2
	BuildDir  string    // e.g. /tmp/distri-build-8123911
	DestDir   string    // e.g. /tmp/distri-dest-3129384/tmp
	Prefix    string    // e.g. /ro/busybox-1.29.2
	Hermetic  bool
	// Debug is one of after-steps, after-install, after-wrapper,
	// after-loopmount, after-elf, after-libfarm
	Debug       string
	FUSE        bool
	ChrootDir   string // only set if Hermetic is enabled
	Jobs        int
	InputDigest string // opaque result of digest()
	Repo        string

	// substituteCache maps from a variable name like ${DISTRI_RESOLVE:expat} to
	// the resolved package name like expat-amd64-2.2.6-1.
	substituteCache map[string]string

	ArtifactWriter io.Writer                                `json:"-"`
	GlobHook       func(imgDir, pkg string) (string, error) `json:"-"`
}

func NewCtx() (*Ctx, error) {
	return &Ctx{
		Arch: "amd64", // TODO
		Repo: env.DefaultRepo,
	}, nil
}

func (b *Ctx) Clone() *Ctx {
	result := &Ctx{}
	// TODO: explicitly copy fields
	*result = *b
	return result
}

const digestDebug = false

func (b *Ctx) Digest() (string, error) {
	if b.InputDigest != "" {
		return b.InputDigest, nil
	}

	h := fnv.New128a()
	h.Write([]byte(proto.MarshalTextString(b.Proto)))

	// Resolve build dependencies.
	//
	// This explicitly does not use Builddeps, which calls GlobAndResolve. In
	// this situation, we must only Glob, not Resolve: non-explicit runtime
	// dependencies are covered by the code path below, and a .meta.textproto
	// might not exist yet (e.g. when doing digests for a batch build).
	bdeps := append(b.Builderdeps(b.Proto), b.Proto.GetDep()...)
	deps, err := b.Glob(b.Repo, bdeps)
	if err != nil {
		return "", fmt.Errorf("glob(%v): %w", b.Pkg, err)
	}
	if digestDebug {
		log.Printf("Digest(%s); deps=%v", b.Pkg, deps)
	}
	h.Write([]byte(strings.Join(deps, ",")))

	for _, cp := range b.Proto.GetCherryPick() {
		fn := filepath.Join(b.PkgDir, cp)
		if digestDebug {
			log.Printf("Digest(%s); patch %v", b.Pkg, fn)
		}
		b, err := ioutil.ReadFile(fn)
		if err != nil {
			return "", err
		}
		h.Write(b)
	}

	// Resolve runtime-deps that go into the build (as opposed to those being
	// discovered during the build, which can only ever reference build-time
	// deps):
	// TODO: also cover the non-split package so that we definitely hit
	// b.Proto.GetRuntimeDep()
	for _, pkg := range b.Proto.GetSplitPackage() {
		deps := append([]string{},
			append(b.Proto.GetRuntimeDep(),
				pkg.GetRuntimeDep()...)...)
		{
			pruned := make([]string, 0, len(deps))
			for _, d := range deps {
				if distri.ParseVersion(d).Pkg == pkg.GetName() {
					continue
				}
				pruned = append(pruned, d)
			}
			deps = pruned
		}
		globbed, err := b.Glob(b.Repo, deps)
		if err != nil {
			return "", err
		}
		if digestDebug {
			log.Printf("Digest(%s); globbed=%v", b.Pkg, globbed)
		}
		h.Write([]byte(strings.Join(globbed, ",")))
	}
	b.InputDigest = fmt.Sprintf("%032x", h.Sum(nil))
	return b.InputDigest, nil
}

func (b *Ctx) FullName() string {
	return b.Pkg + "-" + b.Arch + "-" + b.Version
}

func (b *Ctx) serialize() (string, error) {
	// TODO: exempt the proto field from marshaling, it needs jsonpb once you use oneofs
	enc, err := json.Marshal(b)
	if err != nil {
		return "", err
	}

	tmp, err := ioutil.TempFile("", "distri")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	if _, err := tmp.Write(enc); err != nil {
		return "", err
	}

	return tmp.Name(), tmp.Close()
}

func (b *Ctx) PkgSource() error {
	const subdir = "src"
	src := b.SourceDir
	// Contains e.g. .build-id/63/f308646429e696f78291e5734b6fd83422d8bb.debug
	debugDir := filepath.Join(b.DestDir, b.Prefix, "debug")
	buildDir := func() string {
		if b.Proto.GetWritableSourcedir() {
			return filepath.Join(b.ChrootDir, "usr", "src", b.FullName())
		}
		return filepath.Join(b.ChrootDir, b.BuildDir)
	}()
	_ = buildDir

	var (
		eg errgroup.Group

		allPathsMu sync.Mutex
		allPaths   []string
	)
	prefix := "/usr/src/" + b.FullName() + "/"
	err := filepath.Walk(debugDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		eg.Go(func() error {
			paths, err := dwarfPaths(path)
			if err != nil {
				var decodeErr dwarf.DecodeError
				if errors.As(err, &decodeErr) {
					if decodeErr.Err == "too short" {
						log.Printf("TODO: empty .debug DWARF in %s", b.FullName())
						return nil
					}
				}
				return err
			}
			filtered := make([]string, 0, len(paths))
			for _, p := range paths {
				// Remove paths that reference other compilation units
				// (e.g. /usr/src/glibc-amd64-2.31-4/csu/init.c):
				if !strings.HasPrefix(p, prefix) {
					continue
				}
				filtered = append(filtered, p)
			}
			allPathsMu.Lock()
			defer allPathsMu.Unlock()
			allPaths = append(allPaths, filtered...)
			return nil
		})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := eg.Wait(); err != nil {
		return err
	}

	dest, err := filepath.Abs("../distri/" + subdir + "/" + b.FullName() + ".squashfs")
	if err != nil {
		return err
	}

	// TODO(correctness): switch from modtime to hashing contents
	// the modtime of generated files changes all the time
	// stat, err := os.Stat(dest)
	// if err != nil && !os.IsNotExist(err) {
	// 	return err
	// }
	// if err == nil {
	// 	// Check if the src squashfs image is up to date:
	// 	var (
	// 		latest     time.Time
	// 		latestPath string
	// 	)
	// 	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
	// 		if err != nil {
	// 			return err
	// 		}
	// 		if info.ModTime().After(latest) {
	// 			latest = info.ModTime()
	// 			latestPath = path
	// 		}
	// 		return nil
	// 	})
	// 	if err != nil {
	// 		return err
	// 	}
	// 	for _, p := range allPaths {
	// 		p = filepath.Join(b.ChrootDir, p)
	// 		info, err := os.Stat(p)
	// 		if err != nil {
	// 			return err
	// 		}
	// 		if info.ModTime().After(latest) {
	// 			latest = info.ModTime()
	// 			latestPath = p
	// 		}
	// 	}

	// 	if stat.ModTime().After(latest) {
	// 		log.Printf("src squashfs %s up to date", src)
	// 		return nil // src squashfs up to date
	// 	}
	// 	log.Printf("file %v changed (maybe others), rebuilding src squashfs image", latestPath)
	// }

	f, err := renameio.TempFile("", dest)
	if err != nil {
		return err
	}
	defer f.Cleanup()
	w, err := squashfs.NewWriter(f, time.Now())
	if err != nil {
		return err
	}

	children, err := cpscan(src, "")
	if err != nil {
		return err
	}
	byName := make(map[string]*cpFileInfo)
	for _, child := range children {
		byName[child.fi.Name()] = child
	}
	wrapped := &cpFileInfo{
		byName:   byName,
		children: children,
	}
	if err := func() error {
		for _, p := range allPaths {
			// dir is e.g. /usr/src/debug-amd64-1/subdir/another
			dir := filepath.Dir(p)
			// rel is e.g. subdir/another
			rel := strings.TrimPrefix(dir, "/usr/src/"+b.FullName())
			var cdir *cpFileInfo
			if rel != "" {
				path := strings.TrimPrefix(rel, "/")
				parent := filepath.Join(b.ChrootDir, "/usr/src/"+b.FullName())
				if _, err := wrapped.mkdirAll(path, parent); err != nil {
					return err
				}
				var err error
				cdir, err = wrapped.lookup(strings.TrimPrefix(rel, "/"))
				if err != nil {
					log.Printf("BUG: lookup(rel=%s): %v", rel, err)
					continue
				}
			} else {
				cdir = wrapped
			}
			name := filepath.Base(p)
			if _, ok := cdir.byName[name]; ok {
				continue
			}
			log.Printf("p=%s not present", p)
			info, err := os.Lstat(filepath.Join(b.ChrootDir, p))
			if err != nil {
				return err
			}
			cdir.addChild(&cpFileInfo{
				fi:   info,
				root: buildDir,
				dir:  rel,
			})
		}
		return nil
	}(); err != nil && b.Proto.GetAckMissingDwarf() == "" {
		return err
	}

	for _, child := range wrapped.children {
		if err := child.copyTo(w.Root); err != nil {
			return err
		}
	}
	if err := w.Root.Flush(); err != nil {
		return err
	}

	if err := w.Flush(); err != nil {
		return err
	}

	if err := f.CloseAtomicallyReplace(); err != nil {
		return err
	}
	b.ArtifactWriter.Write([]byte("_build/distri/" + subdir + "/" + b.FullName() + ".squashfs" + "\n"))
	log.Printf("source package successfully created in %s", dest)

	return nil
}

func (b *Ctx) Package() error {
	type splitPackage struct {
		Proto  *pb.SplitPackage
		subdir string
	}
	var pkgs []splitPackage
	for _, pkg := range b.Proto.GetSplitPackage() {
		pkgs = append(pkgs, splitPackage{
			Proto:  pkg,
			subdir: "pkg",
		})
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(b.DestDir), b.FullName(), "debug")); err == nil {
		pkgs = append(pkgs, splitPackage{
			Proto: &pb.SplitPackage{
				Name:  proto.String(b.Pkg),
				Claim: []*pb.Claim{{Glob: proto.String("debug")}},
			},
			subdir: "debug",
		})
	}
	pkgs = append(pkgs, splitPackage{
		Proto: &pb.SplitPackage{
			Name:  proto.String(b.Pkg),
			Claim: []*pb.Claim{{Glob: proto.String("*")}},
		},
		subdir: "pkg",
	})
	for _, pkg := range pkgs {
		log.Printf("packaging %+v", pkg)
		fullName := pkg.Proto.GetName() + "-" + b.Arch + "-" + b.Version
		squashfsName := pkg.subdir + "/" + fullName + ".squashfs"
		pkgEv := trace.Event("pkg "+squashfsName, tidBuildpkg)
		dest, err := filepath.Abs("../distri/" + squashfsName)
		if err != nil {
			return err
		}

		f, err := renameio.TempFile("", dest)
		if err != nil {
			return err
		}
		defer f.Cleanup()
		w, err := squashfs.NewWriter(f, time.Now())
		if err != nil {
			return err
		}

		// Look for files in b.fullName(), i.e. the actual package name
		destRoot := filepath.Join(filepath.Dir(b.DestDir), b.FullName())
		// Place files in fullName, i.e. the split package name
		tmp := filepath.Join(filepath.Dir(b.DestDir), fullName)
		if pkg.subdir != "pkg" {
			// Side-step directory conflict for packages with the same name in a
			// different subdir (e.g. pkg/irssi-amd64-1.1.1.squashfs
			// vs. debug/irssi-amd64-1.1.1.squashfs):
			tmp += "-" + pkg.subdir
		}
		for _, claim := range pkg.Proto.GetClaim() {
			if claim.GetGlob() == "*" {
				// Common path: no globbing or file manipulation required
				continue
			}
			matches, err := filepath.Glob(filepath.Join(destRoot, claim.GetGlob()))
			if err != nil {
				return err
			}
			// Move files from actual package dir to split package dir
			for _, m := range matches {
				rel, err := filepath.Rel(destRoot, m)
				if err != nil {
					return err
				}
				// rel is e.g. out/lib64/libgcc_s.so.1
				dest := filepath.Join(tmp, rel)
				if dir := claim.GetDir(); dir != "" {
					dest = filepath.Join(tmp, dir, filepath.Base(m))
				}
				if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
					return err
				}
				if err := os.Rename(m, dest); err != nil {
					return err
				}
				// TODO: make symlinking the original optional
				rel, err = filepath.Rel(filepath.Dir(m), dest)
				if err != nil {
					return err
				}
				if err := os.Symlink(rel, m); err != nil {
					return err
				}
				b.Proto.RuntimeDep = append(b.Proto.RuntimeDep, fullName)
			}
		}
		if err := cp(w.Root, tmp); err != nil {
			return err
		}

		if err := w.Flush(); err != nil {
			return err
		}

		if err := f.CloseAtomicallyReplace(); err != nil {
			return err
		}
		b.ArtifactWriter.Write([]byte("_build/distri/" + pkg.subdir + "/" + fullName + ".squashfs" + "\n"))
		log.Printf("package successfully created in %s", dest)
		pkgEv.Done()
	}

	return nil
}

func (b *Ctx) fillSubstituteCache(deps []string) {
	cache := make(map[string]string)
	for _, dep := range deps {
		v := distri.ParseVersion(dep)
		if cur, exists := cache[v.Pkg]; !exists || distri.PackageRevisionLess(cur, dep) {
			cache[v.Pkg] = dep
			cache[v.Pkg+"-"+v.Arch] = dep
		}
	}
	b.substituteCache = cache
}

func (b *Ctx) substitute(s string) string {
	// TODO: different format? this might be mistaken for environment variables
	s = strings.ReplaceAll(s, "${DISTRI_DESTDIR}", b.DestDir)
	s = strings.ReplaceAll(s, "${DISTRI_PREFIX}", filepath.Join(b.Prefix, "out"))
	s = strings.ReplaceAll(s, "${DISTRI_BUILDDIR}", b.BuildDir)
	s = strings.ReplaceAll(s, "${DISTRI_SOURCEDIR}", b.SourceDir)
	s = strings.ReplaceAll(s, "${DISTRI_FULLNAME}", b.FullName())
	s = strings.ReplaceAll(s, "${DISTRI_JOBS}", strconv.Itoa(b.Jobs))
	for k, v := range b.substituteCache {
		s = strings.ReplaceAll(s, "${DISTRI_RESOLVE:"+k+"}", v)
	}
	return s
}

func (b *Ctx) substituteStrings(strings []string) []string {
	output := make([]string, len(strings))
	for idx, s := range strings {
		output[idx] = b.substitute(s)
	}
	return output
}

func newerRevisionGoesFirst(deps []string) []string {
	byPkg := make(map[string][]string)
	for _, dep := range deps {
		pv := distri.ParseVersion(dep)
		byPkg[pv.Pkg] = append(byPkg[pv.Pkg], dep)
	}
	for _, versions := range byPkg {
		sort.Slice(versions, func(i, j int) bool {
			vi := distri.ParseVersion(versions[i])
			vj := distri.ParseVersion(versions[j])
			less := vi.DistriRevision < vj.DistriRevision
			return !less // reverse
		})
	}
	result := make([]string, 0, len(deps))
	for _, dep := range deps {
		pv := distri.ParseVersion(dep)
		versions, ok := byPkg[pv.Pkg]
		if !ok {
			continue // already appended earlier
		}
		result = append(result, versions...)
		delete(byPkg, pv.Pkg)
	}
	return result
}

func (b *Ctx) env(deps []string, hermetic bool) []string {
	// TODO: this should go into the C builder once the C builder is used by all packages
	var (
		libDirs       []string
		pkgconfigDirs []string
		includeDirs   []string
		perl5Dirs     []string
		pythonDirs    []string
	)

	// add the package itself, not just its dependencies: the package might
	// install a shared library which it also uses (e.g. systemd).
	deps = append(deps, b.FullName())
	deps = newerRevisionGoesFirst(deps)

	for _, dep := range deps {
		libDirs = append(libDirs, "/ro/"+dep+"/out/lib")
		// TODO: should we try to make programs install to /lib instead? examples: libffi
		libDirs = append(libDirs, "/ro/"+dep+"/out/lib64")
		pkgconfigDirs = append(pkgconfigDirs, "/ro/"+dep+"/out/lib/pkgconfig")
		pkgconfigDirs = append(pkgconfigDirs, "/ro/"+dep+"/out/share/pkgconfig")
		// Exclude glibc from CPATH: it needs to come last (as /usr/include),
		// and gcc doesn’t recognize that the non-system directory glibc-2.27
		// duplicates the system directory /usr/include because we only symlink
		// the contents, not the whole directory.
		if pv := distri.ParseVersion(dep); pv.Pkg != "glibc" && pv.Pkg != "glibc-i686" {
			includeDirs = append(includeDirs, "/ro/"+dep+"/out/include")
			includeDirs = append(includeDirs, "/ro/"+dep+"/out/include/x86_64-linux-gnu")
		}
		perl5Dirs = append(perl5Dirs, "/ro/"+dep+"/out/lib/perl5")
		// TODO: is site-packages the best choice here?
		pythonDirs = append(pythonDirs, "/ro/"+dep+"/out/lib/python3.7/site-packages")
		pythonDirs = append(pythonDirs, "/ro/"+dep+"/out/lib/python2.7/site-packages")
	}

	ifNotHermetic := func(val string) string {
		if !hermetic {
			return val
		}
		return ""
	}

	env := []string{
		// TODO: remove /ro/bin hack for python, file bug: python3 -c 'import sys;print(sys.path)' prints wrong result with PATH=/bin and /bin→/ro/bin and /ro/bin/python3→../python3-3.7.0/bin/python3
		"PATH=/ro/bin:/bin" + ifNotHermetic(":$PATH"),                                              // for finding binaries
		"LIBRARY_PATH=" + strings.Join(libDirs, ":") + ifNotHermetic(":$LIBRARY_PATH"),             // for gcc
		"LD_LIBRARY_PATH=" + strings.Join(libDirs, ":") + ifNotHermetic(":$LD_LIBRARY_PATH"),       // for ld
		"CPATH=" + strings.Join(includeDirs, ":") + ifNotHermetic(":$CPATH"),                       // for gcc
		"PKG_CONFIG_PATH=" + strings.Join(pkgconfigDirs, ":") + ifNotHermetic(":$PKG_CONFIG_PATH"), // for pkg-config
		"PERL5LIB=" + strings.Join(perl5Dirs, ":") + ifNotHermetic(":$PERL5LIB"),                   // for perl
		"PYTHONPATH=" + strings.Join(pythonDirs, ":") + ifNotHermetic(":$PYTHONPATH"),
	}
	// Exclude LDFLAGS for glibc as per
	// https://github.com/Linuxbrew/legacy-linuxbrew/issues/126
	if b.Pkg != "glibc" && b.Pkg != "glibc-i686" {
		env = append(env, "LDFLAGS=-Wl,-rpath="+b.Prefix+"/lib "+
			"-Wl,--dynamic-linker=/ro/"+b.substituteCache["glibc-amd64"]+"/out/lib/ld-linux-x86-64.so.2 "+
			strings.Join(b.Proto.GetCbuilder().GetExtraLdflag(), " ")) // for ld
	}
	return env
}

func (b *Ctx) runtimeEnv(deps []string) []string {
	// TODO: this should go into the C builder once the C builder is used by all packages
	var (
		binDirs    []string
		libDirs    []string
		perl5Dirs  []string
		pythonDirs []string
	)

	// add the package itself, not just its dependencies: the package might
	// install a shared library which it also uses (e.g. systemd).
	deps = append(deps, b.FullName())
	deps = newerRevisionGoesFirst(deps)

	for _, dep := range deps {
		// TODO: these need to be the bindirs of the runtime deps. move wrapper
		// script creation and runtimeEnv call down to when we know runtimeDeps
		binDirs = append(binDirs, "/ro/"+dep+"/bin")
		libDirs = append(libDirs, "/ro/"+dep+"/out/lib")
		// TODO: should we try to make programs install to /lib instead? examples: libffi
		libDirs = append(libDirs, "/ro/"+dep+"/out/lib64")
		perl5Dirs = append(perl5Dirs, "/ro/"+dep+"/out/lib/perl5")
		// TODO: is site-packages the best choice here?
		pythonDirs = append(pythonDirs, "/ro/"+dep+"/out/lib/python3.7/site-packages")
	}

	env := []string{
		"PATH=" + strings.Join(binDirs, ":"),            // for finding binaries
		"LD_LIBRARY_PATH=" + strings.Join(libDirs, ":"), // for ld
		"PERL5LIB=" + strings.Join(perl5Dirs, ":"),      // for perl
		"PYTHONPATH=" + strings.Join(pythonDirs, ":"),   // for python
	}
	return env
}

// Builderdeps returns specified builder’s (e.g. cmake builder, or perl builder)
// dependencies. E.g., the Go builder declares a dependency on golang.
//
// Almost all builders include the C builder’s dependencies, as most languages
// have a C interface.
func (b *Ctx) Builderdeps(p *pb.Build) []string {
	var deps []string
	if builder := p.Builder; builder != nil {
		const native = "amd64" // TODO: configurable / auto-detect
		// The C builder dependencies are re-used by many other builders
		// (anything that supports linking against C libraries).
		nativeDeps := []string{
			// configure runtime dependencies:
			"bash",
			"coreutils",
			"sed",
			"grep",
			"gawk",
			"diffutils",
			"file",
			"pkg-config",

			// C build environment:
			"gcc-libs",
			"mpc",  // TODO: remove once gcc binaries find these via their rpath
			"mpfr", // TODO: remove once gcc binaries find these via their rpath
			"gmp",  // TODO: remove once gcc binaries find these via their rpath
			"make",
			"glibc",
			"linux",
			"findutils", // find(1) is used by libtool, build of e.g. libidn2 will fail if not present
			"musl",      // for wrapper programs

			"strace", // useful for interactive debugging
		}

		if cb, ok := builder.(*pb.Build_Cbuilder); ok && cb.Cbuilder.GetAutoreconf() {
			nativeDeps = append(nativeDeps, []string{
				"autoconf",
				"automake",
				"libtool",
				"gettext",
			}...)
		}

		// TODO: check for native
		if b.Arch == "amd64" {
			nativeDeps = append(nativeDeps, "gcc", "binutils")
		} else {
			nativeDeps = append(nativeDeps,
				"gcc-"+b.Arch,
				"gcc-libs-"+b.Arch,
				"glibc-"+b.Arch,
				"binutils-"+b.Arch,
				// Also make available the native compiler for generating code
				// at build-time, which e.g. libx11 does (via autoconf’s
				// AX_PROG_CC_FOR_BUILD):
				"gcc",
				"binutils",
			)
		}

		cdeps := make([]string, len(nativeDeps))
		for idx, dep := range nativeDeps {
			cdeps[idx] = dep + "-" + native
		}

		switch builder.(type) {
		case *pb.Build_Perlbuilder:
			deps = append(deps, []string{
				"perl-" + native,
			}...)
			deps = append(deps, cdeps...)

		case *pb.Build_Pythonbuilder:
			deps = append(deps, []string{
				"python3-" + native,
			}...)
			deps = append(deps, cdeps...)

		case *pb.Build_Gomodbuilder:
			deps = append(deps, []string{
				"bash-" + native,
				"coreutils-" + native,
			}...)

		case *pb.Build_Gobuilder:
			deps = append(deps, []string{
				"bash-" + native,
				"coreutils-" + native,
				"golang-" + native,
			}...)
			deps = append(deps, cdeps...) // for cgo

		case *pb.Build_Cbuilder:
			deps = append(deps, cdeps...)

		case *pb.Build_Cmakebuilder:
			deps = append(deps, []string{
				"cmake-" + native,
				"ninja-" + native,
			}...)
			deps = append(deps, cdeps...)

		case *pb.Build_Mesonbuilder:
			deps = append(deps, []string{
				"meson-" + native,
			}...)
			deps = append(deps, cdeps...)
		}
	}
	return deps
}

// Builddeps globs and resolves Builderdeps() and proto build dependencies.
func (b *Ctx) Builddeps(p *pb.Build) ([]string, error) {
	// builderdeps must come first so that their ordering survives the resolve
	// call below.
	deps := append(b.Builderdeps(p), p.GetDep()...)
	return b.GlobAndResolve(b.Repo, deps, "")
}

func fuseMkdirAll(ctl string, dir string) error {
	ctl, err := os.Readlink(ctl)
	if err != nil {
		return err
	}

	log.Printf("connecting to %s", ctl)
	ctx := context.Background()
	conn, err := grpc.DialContext(ctx, "unix://"+ctl, grpc.WithBlock(), grpc.WithInsecure())
	if err != nil {
		return err
	}
	cl := pb.NewFUSEClient(conn)
	if _, err := cl.MkdirAll(ctx, &pb.MkdirAllRequest{Dir: proto.String(dir)}); err != nil {
		return err
	}
	return nil
}

var wrapperTmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"envkey": func(env string) string {
		if idx := strings.IndexByte(env, '='); idx > -1 {
			return env[:idx]
		}
		return env
	},
	"envval": func(env string) string {
		if idx := strings.IndexByte(env, '='); idx > -1 {
			return env[idx+1:]
		}
		return env
	},
}).Parse(`
#define _GNU_SOURCE
#include <stdio.h>

#include <err.h>
#include <unistd.h>
#include <stdlib.h>

static char filename[] __attribute__((section("distrifilename"))) = "{{ .Prefix }}/{{ .Bin }}";

int main(int argc, char *argv[]) {
{{ range $idx, $env := .Env }}
  {
    char *dest = "{{ envval $env }}";
    char *env = getenv("{{ envkey $env }}");
    if (env != NULL) {
      if (asprintf(&dest, "%s:%s", "{{ envval $env }}", env) == -1) {
        err(EXIT_FAILURE, "asprintf");
      }
    }
    setenv("{{ envkey $env }}", dest, 1);
  }
{{ end }}

  argv[0] = filename;
  execv(filename, argv);
  return 1;
}
`))

func (b *Ctx) Build(ctx context.Context, buildLog io.Writer) (*pb.Meta, error) {
	if os.Getenv("DISTRI_BUILD_PROCESS") != "1" {
		chrootDir, err := ioutil.TempDir("", "distri-buildchroot")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(chrootDir)
		b.ChrootDir = chrootDir

		// Install build dependencies into /ro
		depsdir := filepath.Join(b.ChrootDir, "ro")
		// TODO: mount() does this, no?
		if err := os.MkdirAll(depsdir, 0755); err != nil {
			return nil, err
		}

		deps, err := b.Builddeps(b.Proto)
		if err != nil {
			return nil, xerrors.Errorf("builddeps: %v", err)
		}

		if b.FUSE {
			ctx, canc := context.WithCancel(ctx)
			defer canc()
			join, err := cmdfuse.Mount(ctx, []string{"-overlays=/bin,/out/lib/pkgconfig,/out/include,/out/share/aclocal,/out/share/gir-1.0,/out/share/mime,/out/gopath,/out/lib/gio,/out/lib/girepository-1.0,/out/share/gettext,/out/lib", "-pkgs=" + strings.Join(deps, ","), depsdir})
			if err != nil {
				return nil, xerrors.Errorf("cmdfuse.Mount: %v", err)
			}
			defer func() {
				ctx, canc := context.WithTimeout(ctx, 5*time.Second)
				defer canc()
				join(ctx)
			}()
			defer fuse.Unmount(depsdir)
		} else {
			for _, dep := range deps {
				cleanup, err := mount([]string{"-root=" + depsdir, dep})
				if err != nil {
					return nil, err
				}
				defer cleanup()
			}
		}
		serialized, err := b.serialize()
		if err != nil {
			return nil, err
		}
		defer os.Remove(serialized)

		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx,
			os.Args[0], "build", "-job="+serialized)
		//"strace", "-fvy", "-o", "/tmp/st", os.Args[0], "build", "-job="+serialized)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags: syscall.CLONE_NEWNS | syscall.CLONE_NEWUSER,
			// In the namespace, map the current effective uid and gid to root,
			// so that we can mount file systems:
			UidMappings: []syscall.SysProcIDMap{
				{ContainerID: 0, HostID: 1000, Size: 1},
			},
			GidMappings: []syscall.SysProcIDMap{
				{ContainerID: 0, HostID: 1000, Size: 1},
			},
		}
		cmd.ExtraFiles = []*os.File{w}
		// TODO: clean the environment
		cmd.Env = append(os.Environ(), "DISTRI_BUILD_PROCESS=1")
		cmd.Stdin = os.Stdin // for interactive debugging
		cmd.Stdout = io.MultiWriter(os.Stdout, buildLog)
		cmd.Stderr = io.MultiWriter(os.Stderr, buildLog)
		if err := cmd.Start(); err != nil {
			return nil, xerrors.Errorf("%v: %w", cmd.Args, err)
		}
		// Close the write end of the pipe in the parent process
		if err := w.Close(); err != nil {
			return nil, err
		}
		c, err := ioutil.ReadAll(r)
		if err != nil {
			return nil, err
		}
		var meta pb.Meta
		if err := proto.Unmarshal(c, &meta); err != nil {
			return nil, err
		}
		if err := cmd.Wait(); err != nil {
			if suggestion := usernsError(); suggestion != "" {
				fmt.Fprintf(os.Stderr, "\n%s\n\n", suggestion)
			}
			return nil, xerrors.Errorf("%v: %w", cmd.Args, err)
		}
		if err := b.PkgSource(); err != nil {
			return nil, err
		}
		return &meta, nil
	}

	// Resolve build dependencies before we chroot, so that we still have access
	// to the meta files.
	deps, err := b.Builddeps(b.Proto)
	if err != nil {
		return nil, err
	}

	{
		// We can only resolve run-time dependecies specified on the
		// build.textproto-level (not automatically discovered ones or those
		// specified on the package level).
		resolved, err := b.GlobAndResolve(b.Repo, b.Proto.GetRuntimeDep(), "")
		if err != nil {
			return nil, err
		}

		b.fillSubstituteCache(append(deps, resolved...))
	}

	// TODO: link /bin to /ro/bin, then set PATH=/ro/bin

	if b.Hermetic {

		// Set up device nodes under /dev:
		{
			dev := filepath.Join(b.ChrootDir, "dev")
			if err := os.MkdirAll(dev, 0755); err != nil {
				return nil, err
			}
			if err := ioutil.WriteFile(filepath.Join(dev, "null"), nil, 0644); err != nil {
				return nil, err
			}
			if err := syscall.Mount("/dev/null", filepath.Join(dev, "null"), "none", syscall.MS_BIND, ""); err != nil {
				return nil, err
			}
		}

		// Set up /etc/passwd (required by e.g. python3):
		{
			etc := filepath.Join(b.ChrootDir, "etc")
			if err := os.MkdirAll(etc, 0755); err != nil {
				return nil, err
			}
			if err := ioutil.WriteFile(filepath.Join(etc, "passwd"), []byte("root:x:0:0:root:/root:/bin/sh"), 0644); err != nil {
				return nil, err
			}
			if err := ioutil.WriteFile(filepath.Join(etc, "group"), []byte("root:x:0"), 0644); err != nil {
				return nil, err
			}
		}

		// We are running in a separate mount namespace now.
		{
			// Make available b.SourceDir as /usr/src/<pkg>-<version> (read-only):
			src := filepath.Join(b.ChrootDir, "usr", "src", b.FullName())
			if err := os.MkdirAll(src, 0755); err != nil {
				return nil, err
			}
			if b.Proto.GetWritableSourcedir() {
				// Otherwise a side effect of a b.BuildDir within /tmp:
				if err := os.MkdirAll(filepath.Join(b.ChrootDir, "/tmp"), 0755); err != nil {
					return nil, err
				}

				// Build under /usr/src, which is distributed via srcfs
				// auto-loading src squashfs images:
				if b.Proto.GetInTreeBuild() {
					b.BuildDir = strings.TrimPrefix(src, b.ChrootDir)
				} else {
					b.BuildDir = strings.TrimPrefix(filepath.Join(src, "build"), b.ChrootDir)
				}

				// Fill b.BuildDir with contents from b.SourceDir:
				cp := exec.CommandContext(ctx, "cp", "-T", "-ar", b.SourceDir+"/", src)
				cp.Stdout = io.MultiWriter(os.Stdout, buildLog)
				cp.Stderr = io.MultiWriter(os.Stderr, buildLog)
				if err := cp.Run(); err != nil {
					return nil, err
				}
			} else {
				if err := bindMount(b.SourceDir, src); err != nil {
					return nil, fmt.Errorf("bindMount(%s, %s): %v", b.SourceDir, src, err)
				}
			}
			b.SourceDir = strings.TrimPrefix(src, b.ChrootDir)

			wrappersSrc := filepath.Join(b.PkgDir, "wrappers")
			if _, err := os.Stat(wrappersSrc); err == nil {
				// Make available b.PkgDir/wrappers as /usr/src/wrappers (read-only):
				wrappers := filepath.Join(b.ChrootDir, "usr", "src", "wrappers")
				if err := os.MkdirAll(wrappers, 0755); err != nil {
					return nil, err
				}
				if err := syscall.Mount(wrappersSrc, wrappers, "none", syscall.MS_BIND|syscall.MS_RDONLY, ""); err != nil {
					return nil, fmt.Errorf("bind mount %s %s: %v", wrappersSrc, wrappers, err)
				}
			}
		}

		{
			prefix := filepath.Join(b.ChrootDir, "ro", b.FullName())
			b.Prefix = strings.TrimPrefix(prefix, b.ChrootDir)

			// Make available b.DestDir as /dest/tmp:
			dst := filepath.Join(b.ChrootDir, "dest", "tmp")
			if err := os.MkdirAll(dst, 0755); err != nil {
				return nil, err
			}
			if err := syscall.Mount(b.DestDir, dst, "none", syscall.MS_BIND, ""); err != nil {
				return nil, xerrors.Errorf("bind mount %s %s: %v", b.DestDir, dst, err)
			}
			b.DestDir = strings.TrimPrefix(dst, b.ChrootDir)

			if _, err := os.Stat(prefix); os.IsNotExist(err) {
				// Bind /dest/tmp to prefix (e.g. /ro/systemd-amd64-239) so that
				// shlibdeps works for binaries which depend on libraries they
				// install.
				if err := fuseMkdirAll(filepath.Join(b.ChrootDir, "ro", "ctl"), b.FullName()); err != nil {
					return nil, xerrors.Errorf("fuseMkdirAll: %v", err)
				}
				if err := syscall.Mount(dst, prefix, "none", syscall.MS_BIND, ""); err != nil {
					return nil, xerrors.Errorf("bind mount %s %s: %v", dst, prefix, err)
				}
			}

			for _, subdir := range []string{"lib", "share"} {
				// Make available /dest/tmp/ro/<pkg>/out/subdir as
				// /dest/tmp/ro/subdir so that packages can install “into”
				// exchange dirs (their shadow copy within $DESTDIR, that is).
				if err := os.MkdirAll(filepath.Join(dst, "ro", b.FullName(), "out", subdir), 0755); err != nil {
					return nil, err
				}
				if err := os.Symlink(
					filepath.Join("/dest/tmp/ro", b.FullName(), "out", subdir), // oldname
					filepath.Join(b.ChrootDir, "dest", "tmp", "ro", subdir)); err != nil {
					return nil, err
				}
			}

			// Symlinks:
			//   /bin → /ro/bin
			//   /usr/bin → /ro/bin (for e.g. /usr/bin/env)
			//   /sbin → /ro/bin (for e.g. linux, which hard-codes /sbin/depmod)
			//   /lib64 → /ro/glibc-amd64-2.27/out/lib for ld-linux-x86-64.so.2
			//   /lib → /ro/glibc-i686-amd64-2.27/out/lib for ld-linux.so.2
			//   /usr/share → /ro/share (for e.g. gobject-introspection)
			//   /usr/lib → /ro/lib (for e.g. python3)

			// TODO: glob glibc? chose newest? error on >1 glibc?
			// TODO: without this, gcc fails to produce binaries. /ro/gcc-amd64-8.2.0-1/out/bin/x86_64-pc-linux-gnu-gcc does not pick up our --dynamic-linker flag apparently
			if err := os.Symlink("/ro/"+b.substituteCache["glibc-amd64"]+"/out/lib", filepath.Join(b.ChrootDir, "lib64")); err != nil {
				return nil, err
			}

			// TODO: test for cross
			if b.Arch != "amd64" {
				// gcc-i686 and binutils-i686 are built with --sysroot=/,
				// meaning they will search for startup files (e.g. crt1.o) in
				// $(sysroot)/lib.
				// TODO: try compiling with --sysroot pointing to /ro/glibc-i686-amd64-2.27/out/lib directly?
				if err := os.Symlink("/ro/"+b.substituteCache["glibc-i686-amd64"]+"/out/lib", filepath.Join(b.ChrootDir, "lib")); err != nil {
					return nil, err
				}
			}

			if !b.FUSE {
				if err := os.Symlink("/ro/"+b.substituteCache["glibc-amd64"]+"/out/lib", filepath.Join(b.ChrootDir, "ro", "lib")); err != nil {
					return nil, err
				}
			} else {
				if err := os.Symlink("/ro/include", filepath.Join(b.ChrootDir, "usr", "include")); err != nil {
					return nil, err
				}
			}

			if err := os.Symlink("/ro/lib", filepath.Join(b.ChrootDir, b.DestDir, "lib")); err != nil {
				return nil, err
			}

			if err := os.Symlink("/ro/share", filepath.Join(b.ChrootDir, "usr", "share")); err != nil {
				return nil, err
			}

			if err := os.Symlink("/ro/lib", filepath.Join(b.ChrootDir, "usr", "lib")); err != nil {
				return nil, err
			}

			for _, bin := range []string{"bin", "usr/bin", "sbin"} {
				if err := os.Symlink("/ro/bin", filepath.Join(b.ChrootDir, bin)); err != nil {
					return nil, err
				}
			}

			if err := os.Setenv("PATH", "/bin"); err != nil {
				return nil, err
			}
		}

		// TODO: just use ioutil.TempDir here
		if err := os.MkdirAll(filepath.Join(b.ChrootDir, b.BuildDir), 0755); err != nil {
			return nil, err
		}

		if err := unix.Chroot(b.ChrootDir); err != nil {
			return nil, err
		}

	} else {
		// We are running in a separate mount namespace now.
		{
			// Make available b.SourceDir as /usr/src/<pkg>-<version> (read-only):
			src := filepath.Join("/usr/src", b.FullName())
			if err := syscall.Mount("tmpfs", "/usr/src", "tmpfs", 0, ""); err != nil {
				return nil, xerrors.Errorf("mount tmpfs /usr/src: %v", err)
			}
			if err := os.MkdirAll(src, 0755); err != nil {
				return nil, err
			}
			if err := syscall.Mount(b.SourceDir, src, "none", syscall.MS_BIND|syscall.MS_RDONLY, ""); err != nil {
				return nil, xerrors.Errorf("bind mount %s %s: %v", b.SourceDir, src, err)
			}
			b.SourceDir = src
		}

		{
			// Make available b.DestDir as /ro/<pkg>-<version>:
			dst := filepath.Join("/ro", "tmp")
			// TODO: get rid of the requirement of having (an empty) /ro exist on the host
			if err := syscall.Mount("tmpfs", "/ro", "tmpfs", 0, ""); err != nil {
				return nil, xerrors.Errorf("mount tmpfs /ro: %v", err)
			}
			if err := os.MkdirAll(dst, 0755); err != nil {
				return nil, err
			}
			if err := syscall.Mount(b.DestDir, dst, "none", syscall.MS_BIND, ""); err != nil {
				return nil, xerrors.Errorf("bind mount %s %s: %v", b.DestDir, dst, err)
			}
			b.DestDir = dst

			prefix := filepath.Join("/ro", b.FullName())
			if err := os.MkdirAll(prefix, 0755); err != nil {
				return nil, err
			}
			b.Prefix = prefix

			// Install build dependencies into /ro

			// TODO: the builder should likely install dependencies as required
			// (e.g. if autotools is detected, bash+coreutils+sed+grep+gawk need to
			// be installed as runtime env, and gcc+binutils+make for building)

			deps, err := b.Builddeps(b.Proto)
			if err != nil {
				return nil, err
			}
			if len(deps) > 0 {
				// TODO: refactor installation
				// if err := install(deps); err != nil {
				// 	return nil, err
				// }
			}

			if err := os.MkdirAll("/ro/bin", 0755); err != nil {
				return nil, err
			}

			if err := os.Symlink("bash", "/ro/bin/sh"); err != nil {
				return nil, err
			}

			if err := os.Setenv("PATH", "/ro/bin:"+os.Getenv("PATH")); err != nil {
				return nil, err
			}

			// XXX

			// if err := os.Setenv("PATH", "/bin"); err != nil {
			// 	return err
			// }

			// if err := syscall.Mount("/ro/bin", "/bin", "none", syscall.MS_BIND, ""); err != nil {
			// 	return xerrors.Errorf("bind mount %s %s: %v", "/ro/bin", "/bin", err)
			// }
		}
	}

	if err := os.Chdir(b.BuildDir); err != nil {
		return nil, err
	}

	env := b.env(deps, true)
	runtimeEnv := b.runtimeEnv(deps)

	steps := b.Proto.GetBuildStep()
	if builder := b.Proto.Builder; builder != nil && len(steps) == 0 {
		switch v := builder.(type) {
		case *pb.Build_Cbuilder:
			var err error
			steps, env, err = b.buildc(b.Proto, v.Cbuilder, env)
			if err != nil {
				return nil, err
			}
		case *pb.Build_Cmakebuilder:
			var err error
			steps, env, err = b.buildcmake(v.Cmakebuilder, env)
			if err != nil {
				return nil, err
			}
		case *pb.Build_Mesonbuilder:
			var err error
			steps, env, err = b.buildmeson(v.Mesonbuilder, env)
			if err != nil {
				return nil, err
			}
		case *pb.Build_Perlbuilder:
			var err error
			steps, env, err = b.buildperl(v.Perlbuilder, env)
			if err != nil {
				return nil, err
			}
		case *pb.Build_Pythonbuilder:
			var err error
			steps, env, err = b.buildpython(v.Pythonbuilder, env)
			if err != nil {
				return nil, err
			}
		case *pb.Build_Gomodbuilder:
			var err error
			steps, env, err = b.buildgomod(v.Gomodbuilder, env)
			if err != nil {
				return nil, err
			}
		case *pb.Build_Gobuilder:
			var err error
			steps, env, err = b.buildgo(v.Gobuilder, env, deps, b.Proto.GetSource())
			if err != nil {
				return nil, err
			}
		default:
			return nil, xerrors.Errorf("BUG: unknown builder")
		}
	}

	if len(steps) == 0 {
		return nil, xerrors.Errorf("build.textproto does not specify Builder nor BuildSteps")
	}

	if b.Hermetic {
		// log.Printf("build environment variables:")
		// for _, kv := range env {
		// 	log.Printf("  %s", kv)
		// }
	}

	b.maybeStartDebugShell("before-steps", env)

	// custom build steps
	times := make([]time.Duration, len(steps))
	for idx, step := range steps {
		start := time.Now()
		cmd := exec.CommandContext(ctx, b.substitute(step.Argv[0]), b.substituteStrings(step.Argv[1:])...)
		if b.Hermetic {
			cmd.Env = env
		}
		log.Printf("build step %d of %d: %v", idx+1, len(steps), cmd.Args)
		cmd.Stdin = os.Stdin // for interactive debugging
		// TODO: logging with io.MultiWriter results in output no longer being colored, e.g. during the systemd build. any workaround?
		cmd.Stdout = io.MultiWriter(os.Stdout, buildLog)
		cmd.Stderr = io.MultiWriter(os.Stderr, buildLog)
		if err := cmd.Run(); err != nil {
			// TODO: ask the user first if they want to debug, and only during interactive builds. detect pty?
			// TODO: ring the bell :)
			log.Printf("build step %v failed (%v), starting debug shell", cmd.Args, err)
			cmd := exec.Command("bash", "-i")
			if b.Hermetic {
				cmd.Env = env
			}
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				log.Printf("debug command failed: %v", err)
			}
			return nil, err
		}
		times[idx] = time.Since(start)
	}
	for idx, step := range steps {
		log.Printf("  step %d: %v (command: %v)", idx, times[idx], step.Argv)
	}

	b.maybeStartDebugShell("after-steps", env)

	// Remove if empty (fails if non-empty):
	for _, subdir := range []string{"lib", "share"} {
		os.Remove(filepath.Join(b.DestDir, b.Prefix, "out", subdir))
	}

	for _, path := range b.Proto.GetInstall().GetDelete() {
		log.Printf("deleting %s", path)
		dest := filepath.Join(b.DestDir, b.Prefix, "out", path)
		if err := os.Remove(dest); err != nil {
			// TODO: if EISDIR, call RemoveAll
			return nil, err
		}
	}

	for _, unit := range b.Proto.GetInstall().GetSystemdUnit() {
		fn := b.substitute(unit)
		if _, err := os.Stat(fn); err != nil {
			return nil, xerrors.Errorf("unit %q: %v", unit, err)
		}
		dest := filepath.Join(b.DestDir, b.Prefix, "out", "lib", "systemd", "system")
		log.Printf("installing systemd unit %q: cp %s %s/", unit, fn, dest)
		if err := os.MkdirAll(dest, 0755); err != nil {
			return nil, err
		}
		if err := copyFile(fn, filepath.Join(dest, filepath.Base(fn))); err != nil {
			return nil, err
		}
	}

	for _, f := range b.Proto.GetInstall().GetFile() {
		fn := filepath.Join(b.SourceDir, f.GetSrcpath())
		dest := filepath.Join(b.DestDir, b.Prefix, "out", f.GetDestpath())
		log.Printf("installing file: cp %s %s/", fn, dest)
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return nil, err
		}
		if err := copyFile(fn, dest); err != nil {
			return nil, err
		}
	}

	for _, link := range b.Proto.GetInstall().GetSymlink() {
		oldname := link.GetOldname()
		newname := link.GetNewname()
		log.Printf("symlinking %s → %s", newname, oldname)
		dest := filepath.Join(b.DestDir, b.Prefix, "out")
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dest, newname)), 0755); err != nil {
			return nil, err
		}
		if err := os.Symlink(oldname, filepath.Join(dest, newname)); err != nil {
			return nil, err
		}
	}

	for _, chmod := range b.Proto.GetInstall().GetChmod() {
		dest := filepath.Join(b.DestDir, b.Prefix, "out")
		name := filepath.Join(dest, chmod.GetName())
		st, err := os.Stat(name)
		if err != nil {
			return nil, err
		}
		m := st.Mode()
		if chmod.GetSetuid() {
			m |= os.ModeSetuid
		}
		mode := os.FileMode(uint32(m))
		log.Printf("setting mode to %o: %s", mode, name)
		if err := os.Chmod(name, mode); err != nil {
			return nil, err
		}
	}

	for _, dir := range b.Proto.GetInstall().GetEmptyDir() {
		log.Printf("creating empty dir %s", dir)
		dest := filepath.Join(b.DestDir, b.Prefix, "out")
		if err := os.MkdirAll(filepath.Join(dest, dir), 0755); err != nil {
			return nil, err
		}
	}

	for _, rename := range b.Proto.GetInstall().GetRename() {
		oldname := rename.GetOldname()
		newname := rename.GetNewname()
		log.Printf("renaming %s → %s", oldname, newname)
		dest := filepath.Join(b.DestDir, b.Prefix, "out")
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dest, newname)), 0755); err != nil {
			return nil, err
		}
		if err := os.Rename(filepath.Join(dest, oldname), filepath.Join(dest, newname)); err != nil {
			return nil, err
		}
	}

	b.maybeStartDebugShell("after-install", env)

	if err := os.MkdirAll(filepath.Join(b.DestDir, b.Prefix, "bin"), 0755); err != nil {
		return nil, err
	}
	for _, dir := range []string{"bin", "sbin"} {
		dir = filepath.Join(b.DestDir, b.Prefix, "out", dir)
		// TODO(performance): read directories directly, don’t care about sorting
		fis, err := ioutil.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, fi := range fis {
			newname := filepath.Join(b.DestDir, b.Prefix, "bin", fi.Name())
			wrapper := filepath.Join("/usr/src/wrappers", fi.Name())
			if _, err := os.Stat(wrapper); err == nil {
				c, err := ioutil.ReadFile(wrapper)
				if err != nil {
					return nil, err
				}
				c = []byte(b.substitute(string(c)))
				if err := ioutil.WriteFile(newname, c, 0755); err != nil {
					return nil, err
				}
			} else {
				oldname := filepath.Join(dir, fi.Name())

				if b.Pkg == "bash" && (fi.Name() == "sh" || fi.Name() == "bash") ||
					b.Pkg == "zsh" && (fi.Name() == "zsh" || strings.HasPrefix(fi.Name(), "zsh-")) {
					// prevent creation of a wrapper script for /bin/sh
					// (wrappers execute /bin/sh) and /bin/bash (dracut uses
					// /bin/bash) by using a symlink instead.
					//
					// zsh must not be wrapped, otherwise setting /bin/zsh as
					// login shell results in shell instances which do not
					// consider themselves to be a login shell.
					oldname, err = filepath.Rel(filepath.Join(b.DestDir, b.Prefix, "bin"), oldname)
					if err != nil {
						return nil, err
					}
					if err := os.Symlink(oldname, newname); err != nil {
						return nil, err
					}
					continue
				}

				oldname, err = filepath.Rel(filepath.Join(b.DestDir, b.Prefix), oldname)
				if err != nil {
					return nil, err
				}
				var buf bytes.Buffer
				if err := wrapperTmpl.Execute(&buf, struct {
					Bin    string
					Prefix string
					Env    []string
				}{
					Bin:    oldname,
					Prefix: b.Prefix,
					Env:    runtimeEnv,
				}); err != nil {
					return nil, err
				}
				f, err := ioutil.TempFile("", "distri-wrapper.*.c")
				if err != nil {
					return nil, err
				}
				if _, err := io.Copy(f, &buf); err != nil {
					return nil, err
				}
				if err := f.Close(); err != nil {
					return nil, err
				}
				// getenv := func(key string) string {
				// 	for _, v := range env {
				// 		idx := strings.IndexByte(v, '=')
				// 		if k := v[:idx]; k != key {
				// 			continue
				// 		}
				// 		return v[idx+1:]
				// 	}
				// 	return ""
				// }
				args := []string{
					"-O3",   // optimize as much as possible
					"-s",    // strip
					"-Wall", // enable all warnings
					"-static",
					"-o", newname,
					f.Name(),
				}
				// NOTE: currently, ldflags only influence dynamic linking,
				// so we just drop all ldflags.
				//
				// if ldflags := strings.TrimSpace(getenv("LDFLAGS")); ldflags != "" {
				// 	args = append(args, strings.Split(ldflags, " ")...)
				// }
				cmd := "musl-gcc"
				if b.Pkg == "musl" ||
					b.Pkg == "gcc" ||
					b.Pkg == "gcc-i686-host" ||
					b.Pkg == "gcc-i686" ||
					b.Pkg == "gcc-i686-c" {
					cmd = "gcc"
				}
				gcc := exec.Command(cmd, args...)
				log.Printf("compiling wrapper program: %v", gcc.Args)
				gcc.Env = env
				gcc.Stderr = os.Stderr
				if err := gcc.Run(); err != nil {
					return nil, err
				}
				if err := os.Remove(f.Name()); err != nil {
					return nil, err
				}
			}
		}
	}

	b.maybeStartDebugShell("after-wrapper", env)

	// Make the finished package available at /ro/<pkg>-<version>, so that
	// patchelf will leave e.g. /ro/systemd-239/out/lib/systemd/ in the
	// RPATH.
	if _, err := os.Stat(filepath.Join(b.DestDir, "ro")); err == nil {
		if _, err := os.Stat(b.Prefix); err == nil {
			if err := syscall.Mount(filepath.Join(b.DestDir, b.Prefix), b.Prefix, "none", syscall.MS_BIND, ""); err != nil {
				return nil, err
			}
		}
	}

	b.maybeStartDebugShell("after-loopmount", env)

	// Find shlibdeps while we’re still in the chroot, so that ldd(1) locates
	// the dependencies.
	depPkgs := make(map[string]bool)
	libs := make(map[libDep]bool)
	destDir := filepath.Join(b.DestDir, b.Prefix)
	ldd := filepath.Join("/ro", b.substituteCache["glibc-amd64"], "out", "bin", "ldd")
	log.Printf("finding shlibdeps with %s", ldd)
	var buf [4]byte
	err = filepath.Walk(destDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err // file could be listed but not opened?!
		}
		defer f.Close()
		if _, err := io.ReadFull(f, buf[:]); err != nil {
			return nil // skip non-ELF files
		}
		if !bytes.Equal(buf[:], []byte("\x7fELF")) {
			return nil
		}
		// TODO: detect whether the binary is statically or dynamically linked (the latter has an INTERP section)

		// We intentionally skip the wrapper program so that relevant
		// environment variables (e.g. LIBRARY_PATH) do not get changed.
		libDeps, err := findShlibDeps(ldd, path, env)
		if err != nil {
			if err == errLddFailed {
				return nil // skip patchelf
			}
			return err
		}
		for _, d := range libDeps {
			depPkgs[d.pkg] = true
			libs[d] = true
		}

		buildid, err := readBuildid(path)
		if err == errBuildIdNotFound {
			log.Printf("no build id in %s, cannot extract debug symbols", path)
			return nil // keep debug symbols, if any
		}
		if err != nil {
			return xerrors.Errorf("readBuildid(%s): %v", path, err)
		}
		debugPath := filepath.Join(destDir, "debug", ".build-id", string(buildid[:2])+"/"+string(buildid[2:])+".debug")
		if err := os.MkdirAll(filepath.Dir(debugPath), 0755); err != nil {
			return err
		}
		objcopy := exec.CommandContext(ctx, "objcopy", "--only-keep-debug", path, debugPath)
		objcopy.Stdout = os.Stdout
		objcopy.Stderr = os.Stderr
		if err := objcopy.Run(); err != nil {
			return xerrors.Errorf("%v: %v", objcopy.Args, err)
		}
		if b.Pkg != "binutils" {
			strip := exec.CommandContext(ctx, "strip", "-g", path)
			strip.Stdout = os.Stdout
			strip.Stderr = os.Stderr
			if err := strip.Run(); err != nil {
				return xerrors.Errorf("%v: %v", strip.Args, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	b.maybeStartDebugShell("after-elf", env)

	// Replace the symlink to /ro/lib with a directory of links to the
	// actually required libraries:
	libDir := filepath.Join(b.DestDir, b.Prefix, "lib")
	if err := os.MkdirAll(libDir, 0755); err != nil {
		return nil, err
	}
	for lib := range libs {
		newname := filepath.Join(libDir, lib.basename)
		oldname, err := filepath.EvalSymlinks(lib.path)
		if err != nil {
			return nil, err
		}
		if err := os.Symlink(oldname, newname); err != nil && !os.IsExist(err) {
			return nil, err
		}
	}

	b.maybeStartDebugShell("after-libfarm", env)

	bin := filepath.Join(destDir, "out", "bin")
	if err := filepath.Walk(bin, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		b, err := ioutil.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(string(b), "#!/ro/") {
			return nil
		}
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		pv := distri.ParseVersion(lines[0])
		if pv.DistriRevision > 0 {
			depPkgs[pv.String()] = true
		}
		return nil
	}); err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	// TODO(optimization): these could be build-time dependencies, as they are
	// only required when building against the library, not when using it.
	pkgconfig := filepath.Join(destDir, "out", "lib", "pkgconfig")
	fis, err := ioutil.ReadDir(pkgconfig)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, fi := range fis {
		b, err := ioutil.ReadFile(filepath.Join(pkgconfig, fi.Name()))
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if !strings.HasPrefix(line, "Requires.private: ") &&
				!strings.HasPrefix(line, "Requires: ") {
				continue
			}
			line = strings.TrimPrefix(line, "Requires:")
			line = strings.TrimPrefix(line, "Requires.private:")
			byPkg := make(map[string]string)
			for _, dep := range deps {
				for _, subdir := range []string{"lib", "share"} {
					fis, err := ioutil.ReadDir(filepath.Join("/ro", dep, "out", subdir, "pkgconfig"))
					if err != nil && !os.IsNotExist(err) {
						return nil, err
					}
					for _, fi := range fis {
						if cur, exists := byPkg[fi.Name()]; !exists || distri.PackageRevisionLess(cur, dep) {
							byPkg[fi.Name()] = dep
						}
					}
				}
			}
			modules := pkgConfigFilesFromRequires(line)
			for _, mod := range modules {
				if dep, ok := byPkg[mod+".pc"]; ok {
					log.Printf("found run-time dependency %s from pkgconfig file", dep)
					depPkgs[dep] = true
				}
			}
		}
	}

	if builder := b.Proto.Builder; builder != nil {
		switch builder.(type) {
		case *pb.Build_Cbuilder:
			// no extra runtime deps
		case *pb.Build_Cmakebuilder:
			// no extra runtime deps
		case *pb.Build_Mesonbuilder:
			// no extra runtime deps
		case *pb.Build_Gomodbuilder:
			// no extra runtime deps
		case *pb.Build_Gobuilder:
			// no extra runtime deps
		case *pb.Build_Perlbuilder:
			depPkgs[b.substituteCache["perl-amd64"]] = true
			// pass through all deps to run-time deps
			// TODO: distinguish test-only deps from actual deps based on Makefile.PL
			for _, pkg := range b.Proto.GetDep() {
				depPkgs[pkg] = true
			}
		case *pb.Build_Pythonbuilder:
			depPkgs[b.substituteCache["python3-amd64"]] = true
		default:
			return nil, xerrors.Errorf("BUG: unknown builder")
		}
	}

	deps = make([]string, 0, len(depPkgs))
	for pkg := range depPkgs {
		// prevent circular runtime dependencies
		if distri.ParseVersion(pkg).Pkg == b.Pkg {
			continue
		}
		deps = append(deps, pkg)
	}
	sort.Strings(deps)
	log.Printf("run-time dependencies: %v", deps)
	return &pb.Meta{
		RuntimeDep: deps,
	}, nil
}

// cherryPick applies src to the extracted sources in tmp. src is either the
// path to a file relative to b.PkgDir (i.e., next to build.textproto), or a
// URL.
func (b *Ctx) cherryPick(src, tmp string) error {
	fn := filepath.Join(b.PkgDir, src)
	if _, err := os.Stat(fn); err != nil {
		return err
	}
	f, err := os.Open(fn)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.Command("patch", "-p1", "--batch", "--set-time", "--set-utc")
	cmd.Dir = tmp
	cmd.Stdin = f
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	if err := cmd.Run(); err != nil {
		return xerrors.Errorf("%v: %v", cmd.Args, err)
	}
	return nil
}

// TrimArchiveSuffix removes file extensions such as .tar, .gz, etc.
func TrimArchiveSuffix(fn string) string {
	for _, suffix := range []string{"gz", "lz", "xz", "bz2", "tar", "tgz", "deb"} {
		fn = strings.TrimSuffix(fn, "."+suffix)
	}
	return fn
}

func (b *Ctx) Extract() error {
	fn := filepath.Base(b.Proto.GetSource())

	u, err := url.Parse(b.Proto.GetSource())
	if err != nil {
		return xerrors.Errorf("url.Parse: %v", err)
	}

	if u.Scheme == "distri+gomod" {
		fn = fn + ".tar.gz"
	}

	_, err = os.Stat(b.SourceDir)
	if err == nil {
		return nil // already extracted
	}

	if !os.IsNotExist(err) {
		return err // directory exists, but can’t access it?
	}

	if err := b.verify(fn); err != nil {
		return xerrors.Errorf("verify: %v", err)
	}

	pwd, err := os.Getwd()
	if err != nil {
		return err
	}
	tmp, err := ioutil.TempDir(pwd, "distri")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if strings.HasSuffix(fn, ".deb") {
		abs, err := filepath.Abs(fn)
		if err != nil {
			return err
		}
		cmd := exec.Command("ar", "x", abs)
		cmd.Dir = tmp
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return xerrors.Errorf("%v: %v", cmd.Args, err)
		}
	} else {
		// TODO(later): extract in pure Go to avoid tar dependency
		cmd := exec.Command("tar", "xf", fn, "--strip-components=1", "--no-same-owner", "-C", tmp)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return xerrors.Errorf("%v: %v", cmd.Args, err)
		}
	}

	if err := b.applyPatches(tmp); err != nil {
		return err
	}

	if err := os.Rename(tmp, b.SourceDir); err != nil {
		return err
	}

	return nil
}

func (b *Ctx) Hash(fn string) (string, error) {
	h := sha256.New()
	f, err := os.Open(fn)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func (b *Ctx) verify(fn string) error {
	if _, err := os.Stat(fn); err != nil {
		if !os.IsNotExist(err) {
			return err // file exists, but can’t access it?
		}

		// TODO(later): calculate hash while downloading to avoid having to read the file
		if err := b.Download(fn); err != nil {
			return xerrors.Errorf("download: %v", err)
		}
	}
	log.Printf("verifying %s", fn)
	sum, err := b.Hash(fn)
	if err != nil {
		return err
	}
	if got, want := sum, b.Proto.GetHash(); got != want {
		return xerrors.Errorf("hash mismatch for %s: got %s, want %s", fn, got, want)
	}
	return nil
}

func (b *Ctx) Download(fn string) error {
	u, err := url.Parse(b.Proto.GetSource())
	if err != nil {
		return xerrors.Errorf("url.Parse: %v", err)
	}

	if u.Scheme == "distri+gomod" {
		return b.downloadGoModule(fn, u.Host+u.Path)
	} else if u.Scheme == "http" || u.Scheme == "https" {
		return b.downloadHTTP(fn)
	} else {
		return xerrors.Errorf("unimplemented URL scheme %q", u.Scheme)
	}
}

func (b *Ctx) downloadGoModule(fn, importPath string) error {
	tmpdir, err := ioutil.TempDir("", "distri-gomod")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)
	gotool := exec.Command("go", "mod", "download", "-json", importPath)
	gotool.Dir = tmpdir
	gotool.Env = []string{
		"GO111MODULE=on",
		"GOPATH=" + tmpdir,
		"GOCACHE=" + filepath.Join(tmpdir, "cache"),
		"PATH=" + os.Getenv("PATH"),
	}
	gotool.Stderr = os.Stderr
	out, err := gotool.Output()
	if err != nil {
		return xerrors.Errorf("%v: %v", gotool.Args, err)
	}
	var modinfo struct {
		Info  string
		GoMod string
		Dir   string
	}
	if err := json.Unmarshal(out, &modinfo); err != nil {
		return err
	}
	// E.g.:
	// Info:  /tmp/distri-gomod767829578/pkg/mod/cache/download/golang.org/x/text/@v/v0.3.0.info
	// GoMod: /tmp/distri-gomod767829578/pkg/mod/cache/download/golang.org/x/text/@v/v0.3.0.mod
	// Dir:   /tmp/distri-gomod767829578/pkg/mod/golang.org/x/text@v0.3.0

	var info struct {
		Time string // the version’s timestamp
	}
	bInfo, err := ioutil.ReadFile(modinfo.Info)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(bInfo, &info); err != nil {
		return xerrors.Errorf("malformed Info file: %v", err)
	}
	t, err := time.Parse(time.RFC3339, info.Time)
	if err != nil {
		return xerrors.Errorf("malformed Time in Info file: %v", err)
	}

	trim := filepath.Clean(tmpdir) + "/"
	prefix := strings.TrimSuffix(fn, ".tar.gz") + "/"
	f, err := os.Create(fn)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, fn := range []string{modinfo.Info, modinfo.GoMod} {
		c, err := ioutil.ReadFile(fn)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:    prefix + strings.TrimPrefix(fn, trim),
			ModTime: t,
			Size:    int64(len(c)),
			Mode:    0644,
		}); err != nil {
			return err
		}
		if _, err := tw.Write(c); err != nil {
			return err
		}
	}
	err = filepath.Walk(modinfo.Dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return xerrors.Errorf("file %q is not regular", path)
		}
		mode := int64(0644)
		if info.Mode()&0700 != 0 {
			mode = 0755
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:    prefix + strings.TrimPrefix(path, trim),
			ModTime: t,
			Size:    info.Size(),
			Mode:    mode,
		}); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		if _, err := io.Copy(tw, in); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}

	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	return nil
}

func (b *Ctx) downloadHTTP(fn string) error {
	// We need to disable compression: with some web servers,
	// http.DefaultTransport’s default compression handling results in an
	// unwanted gunzip step. E.g., http://rpm5.org/files/popt/popt-1.16.tar.gz
	// would be stored as an uncompressed tar file.
	t := *http.DefaultTransport.(*http.Transport)
	t.DisableCompression = true
	c := &http.Client{Transport: &t}
	log.Printf("downloading %s to %s", b.Proto.GetSource(), fn)
	resp, err := c.Get(b.Proto.GetSource())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		return xerrors.Errorf("unexpected HTTP status: got %d (%v), want %d", got, resp.Status, want)
	}
	f, err := os.Create(fn)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return f.Close()
}

func (b *Ctx) applyPatches(tmp string) error {
	for _, u := range b.Proto.GetCherryPick() {
		if err := b.cherryPick(u, tmp); err != nil {
			return xerrors.Errorf("cherry picking %s: %v", u, err)
		}
		log.Printf("cherry picked %s", u)
	}
	for _, ef := range b.Proto.GetExtraFile() {
		// copy the file into tmp
		fn := filepath.Join(b.PkgDir, ef)
		inf, err := os.Open(fn)
		if err != nil {
			return err
		}
		defer inf.Close()
		outf, err := os.Create(filepath.Join(tmp, ef))
		if err != nil {
			return err
		}
		defer outf.Close()
		if _, err := io.Copy(outf, inf); err != nil {
			return err
		}
		if err := outf.Close(); err != nil {
			return err
		}
		if err := inf.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (b *Ctx) MakeEmpty() error {
	if _, err := os.Stat(b.SourceDir); err == nil {
		return nil // already exists
	}
	pwd, err := os.Getwd()
	if err != nil {
		return err
	}
	tmp, err := ioutil.TempDir(pwd, "distri")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	if err := b.applyPatches(tmp); err != nil {
		return err
	}

	return os.Rename(tmp, b.SourceDir)
}
