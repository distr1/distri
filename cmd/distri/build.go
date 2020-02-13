package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/distr1/distri"
	cmdfuse "github.com/distr1/distri/cmd/distri/internal/fuse"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/internal/oninterrupt"
	"github.com/distr1/distri/internal/squashfs"
	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"
	"github.com/google/renameio"
	"github.com/jacobsa/fuse"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
	"golang.org/x/xerrors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bpb "github.com/distr1/distri/pb/builder"
)

const buildHelp = `distri build [-flags]

Build a distri package.

Example:
  % distri build -pkg=i3status
`

type buildctx struct {
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
	Debug     bool
	FUSE      bool
	ChrootDir string // only set if Hermetic is enabled

	// substituteCache maps from a variable name like ${DISTRI_RESOLVE:expat} to
	// the resolved package name like expat-amd64-2.2.6-1.
	substituteCache map[string]string

	artifactWriter io.Writer
}

func buildpkg(hermetic, debug, fuse bool, cross, remote string, artifactFd int) error {
	c, err := ioutil.ReadFile("build.textproto")
	if err != nil {
		return err
	}
	var buildProto pb.Build
	if err := proto.UnmarshalText(string(c), &buildProto); err != nil {
		return err
	}

	pwd, err := os.Getwd()
	if err != nil {
		return err
	}

	if cross == "" {
		cross = "amd64" // TODO: configurable / auto-detect
	}

	b := &buildctx{
		Proto:          &buildProto,
		PkgDir:         pwd,
		Pkg:            filepath.Base(pwd),
		Arch:           cross,
		Version:        buildProto.GetVersion(),
		Hermetic:       hermetic,
		FUSE:           fuse,
		Debug:          debug,
		artifactWriter: ioutil.Discard,
	}

	if artifactFd > -1 {
		b.artifactWriter = os.NewFile(uintptr(artifactFd), "")
	}

	// Set fields from the perspective of an installed package so that variable
	// substitution works within wrapper scripts.
	b.Prefix = "/ro/" + b.fullName() // e.g. /ro/hello-amd64-1

	{
		tmpdir, err := ioutil.TempDir("", "distri-dest")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmpdir)
		b.DestDir = filepath.Join(tmpdir, "tmp")
	}

	pkgbuilddir := filepath.Join("../../build/", b.Pkg)

	if err := os.MkdirAll(pkgbuilddir, 0755); err != nil {
		return err
	}
	if err := os.Chdir(pkgbuilddir); err != nil {
		return err
	}

	log.Printf("building %s", b.fullName())

	b.SourceDir = trimArchiveSuffix(filepath.Base(b.Proto.GetSource()))

	u, err := url.Parse(b.Proto.GetSource())
	if err != nil {
		return xerrors.Errorf("url.Parse: %v", err)
	}

	if u.Scheme == "distriroot" {
		if err := updateFromDistriroot(b.SourceDir); err != nil {
			return xerrors.Errorf("updateFromDistriroot: %v", err)
		}
	} else if u.Scheme == "empty" {
		b.SourceDir = "empty"
		if err := b.makeEmpty(); err != nil {
			return xerrors.Errorf("makeEmpty: %v", err)
		}
	} else {
		if err := b.extract(); err != nil {
			return xerrors.Errorf("extract: %v", err)
		}
	}

	b.SourceDir, err = filepath.Abs(b.SourceDir)
	if err != nil {
		return err
	}

	{
		tmpdir, err := ioutil.TempDir("", "distri-build")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmpdir)
		b.BuildDir = tmpdir
	}

	// TODO: remove this, files are installed into b.DestDir + prefix?
	if err := os.MkdirAll(filepath.Join(b.DestDir, "out"), 0755); err != nil {
		return err
	}

	if remote != "" {
		log.Printf("building on %s", remote)

		ctx := context.Background()
		conn, err := grpc.DialContext(ctx, remote, grpc.WithInsecure(), grpc.WithBlock())
		if err != nil {
			return err
		}
		cl := bpb.NewBuildClient(conn)

		deps, err := b.builddeps(b.Proto)
		if err != nil {
			return xerrors.Errorf("builddeps: %v", err)
		}

		alldeps := make([]string, len(deps))
		copy(alldeps, deps)
		alldeps = append(alldeps, b.Proto.GetRuntimeDep()...)
		globbed, err := b.glob(env.DefaultRepo, alldeps)
		if err != nil {
			return err
		}
		resolved, err := resolve(env.DefaultRepo, globbed, "")
		if err != nil {
			return err
		}
		expanded := make([]string, 0, 2*len(resolved))
		for _, r := range resolved {
			expanded = append(expanded, r+".meta.textproto")
			expanded = append(expanded, r+".squashfs")
		}

		prefixed := make([]string, len(expanded))
		for i, e := range expanded {
			prefixed[i] = "build/distri/pkg/" + e
		}

		inputs := append([]string{
			"pkgs/" + b.Pkg + "/build.textproto",
		}, prefixed...)
		for _, input := range inputs {
			log.Printf("store(%s)", input)
			if err := store(ctx, cl, input); err != nil {
				return err
			}
		}

		var artifacts []string
		bcl, err := cl.Build(ctx, &bpb.BuildRequest{
			WorkingDirectory: "pkgs/" + b.Pkg,
			InputPath:        inputs,
		})
		if err != nil {
			return err
		}
		for {
			progress, err := bcl.Recv()
			if err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
			artifacts = append(artifacts, progress.GetOutputPath()...)
			log.Printf("progress: %+v", progress)
		}

		for _, fn := range artifacts {
			log.Printf("retrieve(%s)", fn)
			downcl, err := cl.Retrieve(ctx, &bpb.RetrieveRequest{
				Path: fn,
			})
			if err != nil {
				return err
			}
			var f *renameio.PendingFile
			for {
				chunk, err := downcl.Recv()
				if err != nil {
					if err == io.EOF {
						break
					}
					return err
				}
				if f == nil {
					if chunk.GetPath() == "" {
						return fmt.Errorf("protocol error: first chunk did not contain a path")
					}
					f, err = renameio.TempFile("", filepath.Join(env.DistriRoot, chunk.GetPath()))
					if err != nil {
						return err
					}
					defer f.Cleanup()
				}
				if _, err := f.Write(chunk.GetChunk()); err != nil {
					return err
				}
			}
			if err := f.CloseAtomicallyReplace(); err != nil {
				return err
			}
		}

		return nil
	}

	// concurrently create source squashfs image
	var srcEg errgroup.Group
	srcEg.Go(b.pkgSource)

	meta, err := b.build()
	if err != nil {
		return xerrors.Errorf("build: %v", err)
	}

	if err := setCaps(); err != nil {
		return err
	}

	for _, cap := range b.Proto.GetInstall().GetCapability() {
		setcap := exec.Command("setcap", cap.GetCapability(), cap.GetFilename())
		setcap.Dir = filepath.Join(b.DestDir, b.Prefix, "out")
		log.Printf("%v in %v", setcap.Args, setcap.Dir)
		setcap.Stdout = os.Stdout
		setcap.Stderr = os.Stderr
		setcap.SysProcAttr = &syscall.SysProcAttr{
			AmbientCaps: []uintptr{CAP_SETFCAP},
		}
		if err := setcap.Run(); err != nil {
			return err
		}
	}

	// b.DestDir is /tmp/distri-dest123/tmp
	// package installs into b.DestDir/ro/hello-1

	rel := b.fullName()

	destDir := filepath.Join(filepath.Dir(b.DestDir), rel) // e.g. /tmp/distri-dest123/hello-1

	// rename destDir/tmp/ro/hello-1 to destDir/hello-1:
	if err := os.Rename(filepath.Join(b.DestDir, "ro", rel), destDir); err != nil {
		return err
	}

	// rename destDir/tmp/etc to destDir/etc
	if err := os.Rename(filepath.Join(b.DestDir, "etc"), filepath.Join(destDir, "etc")); err != nil && !os.IsNotExist(err) {
		return err
	}

	if err := b.pkg(); err != nil {
		return err
	}

	pkgs := append(b.Proto.GetSplitPackage(), &pb.SplitPackage{
		Name:  proto.String(b.Pkg),
		Claim: []*pb.Claim{{Glob: proto.String("*")}},
	})
	for _, pkg := range pkgs {
		fullName := pkg.GetName() + "-" + b.Arch + "-" + b.Version

		deps := append(meta.GetRuntimeDep(),
			append(b.Proto.GetRuntimeDep(),
				pkg.GetRuntimeDep()...)...)

		deps, err = b.glob(env.DefaultRepo, deps)
		if err != nil {
			return err
		}

		resolved, err := resolve(env.DefaultRepo, deps, pkg.GetName())
		if err != nil {
			return err
		}

		// TODO: add the transitive closure of runtime dependencies

		log.Printf("%s runtime deps: %q", pkg.GetName(), resolved)

		unions := make([]*pb.Union, len(b.Proto.RuntimeUnion))
		for idx, o := range b.Proto.RuntimeUnion {
			globbed, err := b.glob1(env.DefaultRepo, o.GetPkg())
			if err != nil {
				return err
			}

			unions[idx] = &pb.Union{
				Dir: o.Dir,
				Pkg: proto.String(globbed),
			}
		}

		c := proto.MarshalTextString(&pb.Meta{
			RuntimeDep:   resolved,
			SourcePkg:    proto.String(b.Pkg),
			Version:      proto.String(b.Version),
			RuntimeUnion: unions,
		})
		fn := filepath.Join("../distri/pkg/" + fullName + ".meta.textproto")
		b.artifactWriter.Write([]byte("build/" + strings.TrimPrefix(fn, "../") + "\n"))
		if err := renameio.WriteFile(fn, []byte(c), 0644); err != nil {
			return err
		}
		if err := renameio.Symlink(fullName+".meta.textproto", filepath.Join("../distri/pkg/"+pkg.GetName()+"-"+b.Arch+".meta.textproto")); err != nil {
			return err
		}
	}

	if err := srcEg.Wait(); err != nil {
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

func (b *buildctx) fullName() string {
	return b.Pkg + "-" + b.Arch + "-" + b.Version
}

func (b *buildctx) serialize() (string, error) {
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

func (b *buildctx) pkgSource() error {
	const subdir = "src"
	dest, err := filepath.Abs("../distri/" + subdir + "/" + b.fullName() + ".squashfs")
	if err != nil {
		return err
	}

	stat, err := os.Stat(dest)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		// Check if the src squashfs image is up to date:
		var (
			latest     time.Time
			latestPath string
		)
		err := filepath.Walk(b.SourceDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.ModTime().After(latest) {
				latest = info.ModTime()
				latestPath = path
			}
			return nil
		})
		if err != nil {
			return err
		}
		if stat.ModTime().After(latest) {
			return nil // src squashfs up to date
		}
		log.Printf("file %v changed (maybe others), rebuilding src squashfs image", latestPath)
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

	if err := cp(w.Root, b.SourceDir); err != nil {
		return err
	}

	if err := w.Flush(); err != nil {
		return err
	}

	if err := f.CloseAtomicallyReplace(); err != nil {
		return err
	}
	b.artifactWriter.Write([]byte("build/distri/" + subdir + "/" + b.fullName() + ".squashfs" + "\n"))
	log.Printf("source package successfully created in %s", dest)

	return nil
}

func (b *buildctx) pkg() error {
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
	if _, err := os.Stat(filepath.Join(filepath.Dir(b.DestDir), b.fullName(), "debug")); err == nil {
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
		dest, err := filepath.Abs("../distri/" + pkg.subdir + "/" + fullName + ".squashfs")
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
		destRoot := filepath.Join(filepath.Dir(b.DestDir), b.fullName())
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
		b.artifactWriter.Write([]byte("build/distri/" + pkg.subdir + "/" + fullName + ".squashfs" + "\n"))
		log.Printf("package successfully created in %s", dest)
	}

	return nil
}

func (b *buildctx) fillSubstituteCache(deps []string) {
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

func (b *buildctx) substitute(s string) string {
	// TODO: different format? this might be mistaken for environment variables
	s = strings.ReplaceAll(s, "${DISTRI_DESTDIR}", b.DestDir)
	s = strings.ReplaceAll(s, "${DISTRI_PREFIX}", filepath.Join(b.Prefix, "out"))
	s = strings.ReplaceAll(s, "${DISTRI_BUILDDIR}", b.BuildDir)
	s = strings.ReplaceAll(s, "${DISTRI_SOURCEDIR}", b.SourceDir)
	s = strings.ReplaceAll(s, "${DISTRI_FULLNAME}", b.fullName())
	s = strings.ReplaceAll(s, "${DISTRI_JOBS}", strconv.Itoa(runtime.NumCPU()))
	for k, v := range b.substituteCache {
		s = strings.ReplaceAll(s, "${DISTRI_RESOLVE:"+k+"}", v)
	}
	return s
}

func (b *buildctx) substituteStrings(strings []string) []string {
	output := make([]string, len(strings))
	for idx, s := range strings {
		output[idx] = b.substitute(s)
	}
	return output
}

func depLess(i, j string) bool {
	vi := distri.ParseVersion(i)
	vj := distri.ParseVersion(j)
	if vi.Pkg != vj.Pkg {
		return false // keep order
	}
	return vi.DistriRevision >= vj.DistriRevision
}

func (b *buildctx) env(deps []string, hermetic bool) []string {
	// TODO: this should go into the C builder once the C builder is used by all packages
	var (
		libDirs       []string
		pkgconfigDirs []string
		includeDirs   []string
		perl5Dirs     []string
		pythonDirs    []string
	)

	sort.SliceStable(deps, func(i, j int) bool {
		return depLess(deps[i], deps[j])
	})

	// add the package itself, not just its dependencies: the package might
	// install a shared library which it also uses (e.g. systemd).
	for _, dep := range append(deps, b.fullName()) {
		libDirs = append(libDirs, "/ro/"+dep+"/out/lib")
		// TODO: should we try to make programs install to /lib instead? examples: libffi
		libDirs = append(libDirs, "/ro/"+dep+"/out/lib64")
		pkgconfigDirs = append(pkgconfigDirs, "/ro/"+dep+"/out/lib/pkgconfig")
		pkgconfigDirs = append(pkgconfigDirs, "/ro/"+dep+"/out/share/pkgconfig")
		// Exclude glibc from CPATH: it needs to come last (as /usr/include),
		// and gcc doesn’t recognize that the non-system directory glibc-2.27
		// duplicates the system directory /usr/include because we only symlink
		// the contents, not the whole directory.
		if dep != "glibc-amd64-2.27-4" && dep != "glibc-i686-amd64-2.27-4" &&
			dep != "glibc-amd64-2.27-3" && dep != "glibc-i686-amd64-2.27-3" &&
			dep != "glibc-amd64-2.27-2" && dep != "glibc-i686-amd64-2.27-2" &&
			dep != "glibc-amd64-2.27-1" && dep != "glibc-i686-amd64-2.27-1" &&
			dep != "glibc-amd64-2.27" && dep != "glibc-i686-amd64-2.27" {
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

func (b *buildctx) runtimeEnv(deps []string) []string {
	// TODO: this should go into the C builder once the C builder is used by all packages
	var (
		binDirs    []string
		libDirs    []string
		perl5Dirs  []string
		pythonDirs []string
	)

	sort.SliceStable(deps, func(i, j int) bool {
		return depLess(deps[i], deps[j])
	})

	// add the package itself, not just its dependencies: the package might
	// install a shared library which it also uses (e.g. systemd).
	for _, dep := range append(deps, b.fullName()) {
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

func (b *buildctx) glob1(imgDir, pkg string) (string, error) {
	if st, err := os.Lstat(filepath.Join(imgDir, pkg+".meta.textproto")); err == nil && st.Mode().IsRegular() {
		return pkg, nil // pkg already contains the version
	}
	pkgPattern := pkg
	if suffix, ok := distri.HasArchSuffix(pkg); !ok {
		pkgPattern = pkgPattern + "-" + b.Arch
	} else {
		pkg = strings.TrimSuffix(pkg, "-"+suffix)
	}

	pattern := filepath.Join(imgDir, pkgPattern+"-*.meta.textproto")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	var candidates []string
	for _, m := range matches {
		if st, err := os.Lstat(m); err != nil || !st.Mode().IsRegular() {
			continue
		}
		candidates = append(candidates, strings.TrimSuffix(filepath.Base(m), ".meta.textproto"))
	}
	if len(candidates) > 1 {
		// default to the most recent package revision. If building against an
		// older version is desired, that version must be specified explicitly.
		sort.Slice(candidates, func(i, j int) bool {
			return distri.PackageRevisionLess(candidates[i], candidates[j])
		})
		return candidates[len(candidates)-1], nil
	}
	if len(candidates) == 0 {
		if !b.Hermetic {
			// no package found, fall back to host tools in non-hermetic mode
			return "", nil
		}
		return "", xerrors.Errorf("package %q not found (pattern %s)", pkg, pattern)
	}
	return candidates[0], nil
}

func (b *buildctx) glob(imgDir string, pkgs []string) ([]string, error) {
	globbed := make([]string, 0, len(pkgs))
	for _, pkg := range pkgs {
		tmp, err := b.glob1(imgDir, pkg)
		if err != nil {
			return nil, err
		}
		if tmp == "" {
			continue
		}
		globbed = append(globbed, tmp)
	}
	return globbed, nil
}

func resolve1(imgDir string, pkg string, seen map[string]bool, prune string) ([]string, error) {
	if distri.ParseVersion(pkg).Pkg == prune {
		return nil, nil
	}
	resolved := []string{pkg}
	meta, err := pb.ReadMetaFile(filepath.Join(imgDir, pkg+".meta.textproto"))
	if err != nil {
		return nil, err
	}
	for _, dep := range meta.GetRuntimeDep() {
		if dep == pkg {
			continue // skip circular dependencies, e.g. gcc depends on itself
		}
		if seen[dep] {
			continue
		}
		seen[dep] = true
		r, err := resolve1(imgDir, dep, seen, prune)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, r...)
	}
	return resolved, nil
}

// resolve returns the transitive closure of runtime dependencies for the
// specified packages.
//
// E.g., if iptables depends on libnftnl, which depends on libmnl,
// resolve("iptables") will return ["iptables", "libnftnl", "libmnl"].
func resolve(imgDir string, pkgs []string, prune string) ([]string, error) {
	var resolved []string
	seen := make(map[string]bool)
	for _, pkg := range pkgs {
		if seen[pkg] {
			continue // a recursive call might have found this package already
		}
		seen[pkg] = true
		r, err := resolve1(imgDir, pkg, seen, prune)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, r...)
	}
	return resolved, nil
}

func (b *buildctx) builderdeps(p *pb.Build) []string {
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
				"golang113beta1-" + native,
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

func (b *buildctx) builddeps(p *pb.Build) ([]string, error) {
	// builderdeps must come first so that their ordering survives the resolve
	// call below.
	deps := append(b.builderdeps(p), p.GetDep()...)
	var err error
	deps, err = b.glob(env.DefaultRepo, deps)
	if err != nil {
		return nil, err
	}
	return resolve(env.DefaultRepo, deps, "")
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

func store(ctx context.Context, cl bpb.BuildClient, fn string) error {
	f, err := os.Open(filepath.Join(env.DistriRoot, fn))
	if err != nil {
		return err
	}
	defer f.Close()

	upcl, err := cl.Store(ctx)
	if err != nil {
		return err
	}
	path := fn
	var buf [4096]byte
	for {
		n, err := f.Read(buf[:])
		if err != nil {
			if err == io.EOF {
				break
			}
			return xerrors.Errorf("Read: %v", err)
		}
		chunk := buf[:n]
		if err := upcl.Send(&bpb.Chunk{
			Path:  path,
			Chunk: chunk,
		}); err != nil {
			if status.Code(err) == codes.AlreadyExists {
				return nil
			}
			if err == io.EOF {
				return nil // server closed stream, file already present
			}
			return xerrors.Errorf("Send: %v", err)
		}
		path = ""
	}
	if _, err := upcl.CloseAndRecv(); err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return nil
		}
		return xerrors.Errorf("CloseAndRecv: %v", err)
	}
	return nil
}

func (b *buildctx) build() (*pb.Meta, error) {
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

		deps, err := b.builddeps(b.Proto)
		if err != nil {
			return nil, xerrors.Errorf("builddeps: %v", err)
		}

		if b.FUSE {
			join, err := cmdfuse.Mount([]string{"-overlays=/bin,/out/lib/pkgconfig,/out/include,/out/share/aclocal,/out/share/gir-1.0,/out/share/mime,/out/gopath,/out/lib/gio,/out/lib/girepository-1.0,/out/share/gettext,/out/lib", "-pkgs=" + strings.Join(deps, ","), depsdir})
			if err != nil {
				return nil, xerrors.Errorf("cmdfuse.Mount: %v", err)
			}
			ctx, canc := context.WithTimeout(context.Background(), 5*time.Second)
			defer canc()
			defer join(ctx)
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

		// TODO(later): get rid of unshare dependency, re-implement in pure Go
		// TODO(later): proper error message telling people to sysctl -w kernel.unprivileged_userns_clone=1
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		cmd := exec.Command("unshare",
			"--user",
			"--map-root-user", // for mount permissions in the namespace
			"--mount",
			"--",
			os.Args[0], "build", "-job="+serialized)
		//"strace", "-f", "-o", "/tmp/st", os.Args[0], "-job="+serialized)
		cmd.ExtraFiles = []*os.File{w}
		// TODO: clean the environment
		cmd.Env = append(os.Environ(), "DISTRI_BUILD_PROCESS=1")
		cmd.Stdin = os.Stdin // for interactive debugging
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
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
			fmt.Fprintf(os.Stderr, "\ntry sysctl -w kernel.unprivileged_userns_clone=1? (wild guess)\n\n")
			return nil, xerrors.Errorf("%v: %w", cmd.Args, err)
		}
		return &meta, nil
	}

	logDir := filepath.Dir(b.SourceDir) // TODO: introduce a struct field
	buildLog, err := os.Create(filepath.Join(logDir, "build-"+b.Version+".log"))
	if err != nil {
		return nil, err
	}
	defer buildLog.Close()

	// Resolve build dependencies before we chroot, so that we still have access
	// to the meta files.
	deps, err := b.builddeps(b.Proto)
	if err != nil {
		return nil, err
	}

	{
		// We can only resolve run-time dependecies specified on the
		// build.textproto-level (not automatically discovered ones or those
		// specified on the package level).
		runtimeDeps, err := b.glob(env.DefaultRepo, b.Proto.GetRuntimeDep())
		if err != nil {
			return nil, err
		}

		resolved, err := resolve(env.DefaultRepo, runtimeDeps, "")
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
			src := filepath.Join(b.ChrootDir, "usr", "src", b.fullName())
			if err := os.MkdirAll(src, 0755); err != nil {
				return nil, err
			}
			if err := syscall.Mount(b.SourceDir, src, "none", syscall.MS_BIND|syscall.MS_RDONLY, ""); err != nil {
				return nil, xerrors.Errorf("bind mount %s %s: %v", b.SourceDir, src, err)
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
					return nil, xerrors.Errorf("bind mount %s %s: %v", wrappersSrc, wrappers, err)
				}
			}
		}

		{
			prefix := filepath.Join(b.ChrootDir, "ro", b.fullName())
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
				if err := fuseMkdirAll(filepath.Join(b.ChrootDir, "ro", "ctl"), b.fullName()); err != nil {
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
				if err := os.MkdirAll(filepath.Join(dst, "ro", b.fullName(), "out", subdir), 0755); err != nil {
					return nil, err
				}
				if err := os.Symlink(
					filepath.Join("/dest/tmp/ro", b.fullName(), "out", subdir), // oldname
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
			src := filepath.Join("/usr/src", b.fullName())
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

			prefix := filepath.Join("/ro", b.fullName())
			if err := os.MkdirAll(prefix, 0755); err != nil {
				return nil, err
			}
			b.Prefix = prefix

			// Install build dependencies into /ro

			// TODO: the builder should likely install dependencies as required
			// (e.g. if autotools is detected, bash+coreutils+sed+grep+gawk need to
			// be installed as runtime env, and gcc+binutils+make for building)

			deps, err := b.builddeps(b.Proto)
			if err != nil {
				return nil, err
			}
			if len(deps) > 0 {
				if err := install(deps); err != nil {
					return nil, err
				}
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
			steps, env, err = b.buildc(v.Cbuilder, env)
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
	// custom build steps
	times := make([]time.Duration, len(steps))
	for idx, step := range steps {
		start := time.Now()
		cmd := exec.Command(b.substitute(step.Argv[0]), b.substituteStrings(step.Argv[1:])...)
		if b.Hermetic {
			cmd.Env = env
		}
		log.Printf("build step %d of %d: %v", idx, len(steps), cmd.Args)
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

	if b.Debug {
		log.Printf("starting debug shell because -debug is enabled")
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
	}

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

				if b.Pkg == "bash" && (fi.Name() == "sh" || fi.Name() == "bash") {
					// prevent creation of a wrapper script for /bin/sh
					// (wrappers execute /bin/sh) and /bin/bash (dracut uses
					// /bin/bash) by using a symlink instead:
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

	// Find shlibdeps while we’re still in the chroot, so that ldd(1) locates
	// the dependencies.
	depPkgs := make(map[string]bool)
	libs := make(map[libDep]bool)
	destDir := filepath.Join(b.DestDir, b.Prefix)
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
		ldd := filepath.Join("/ro", b.substituteCache["glibc-amd64"], "out", "bin", "ldd")
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
			return nil // keep debug symbols, if any
		}
		if err != nil {
			return xerrors.Errorf("readBuildid(%s): %v", path, err)
		}
		debugPath := filepath.Join(destDir, "debug", ".build-id", string(buildid[:2])+"/"+string(buildid[2:])+".debug")
		if err := os.MkdirAll(filepath.Dir(debugPath), 0755); err != nil {
			return err
		}
		objcopy := exec.Command("objcopy", "--only-keep-debug", path, debugPath)
		objcopy.Stdout = os.Stdout
		objcopy.Stderr = os.Stderr
		if err := objcopy.Run(); err != nil {
			return xerrors.Errorf("%v: %v", objcopy.Args, err)
		}
		if b.Pkg != "binutils" {
			strip := exec.Command("strip", "-g", path)
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
func (b *buildctx) cherryPick(src, tmp string) error {
	fn := filepath.Join(b.PkgDir, src)
	if _, err := os.Stat(fn); err == nil {
		f, err := os.Open(fn)
		if err != nil {
			return err
		}
		defer f.Close()
		cmd := exec.Command("patch", "-p1", "--batch", "--set-time", "--set-utc")
		cmd.Dir = tmp
		cmd.Stdin = f
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return xerrors.Errorf("%v: %v", cmd.Args, err)
		}
		return nil
	}
	// TODO: remove the URL support. we want patches to be committed alongside the packaging
	resp, err := http.Get(src)
	if err != nil {
		return err
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		return xerrors.Errorf("HTTP status %v", resp.Status)
	}
	// TODO: once we extract in golang tar, we can just re-extract the timestamps
	cmd := exec.Command("patch", "-p1", "--batch", "--set-time", "--set-utc")
	cmd.Dir = tmp
	cmd.Stdin = resp.Body
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return xerrors.Errorf("%v: %v", cmd.Args, err)
	}
	return nil
}

func trimArchiveSuffix(fn string) string {
	for _, suffix := range []string{"gz", "lz", "xz", "bz2", "tar", "tgz", "deb"} {
		fn = strings.TrimSuffix(fn, "."+suffix)
	}
	return fn
}

func (b *buildctx) extract() error {
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

func (b *buildctx) verify(fn string) error {
	if _, err := os.Stat(fn); err != nil {
		if !os.IsNotExist(err) {
			return err // file exists, but can’t access it?
		}

		// TODO(later): calculate hash while downloading to avoid having to read the file
		if err := b.download(fn); err != nil {
			return xerrors.Errorf("download: %v", err)
		}
	}
	log.Printf("verifying %s", fn)
	h := sha256.New()
	f, err := os.Open(fn)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	sum := fmt.Sprintf("%x", h.Sum(nil))
	if got, want := sum, b.Proto.GetHash(); got != want {
		return xerrors.Errorf("hash mismatch for %s: got %s, want %s", fn, got, want)
	}
	return nil
}

func (b *buildctx) download(fn string) error {
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

func (b *buildctx) downloadGoModule(fn, importPath string) error {
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

func (b *buildctx) downloadHTTP(fn string) error {
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

func (b *buildctx) applyPatches(tmp string) error {
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

func (b *buildctx) makeEmpty() error {
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

func updateFromDistriroot(builddir string) error {
	// TODO(later): fill ignore from .gitignore?
	ignore := map[string]bool{
		".git":         true,
		"build":        true,
		"linux-4.18.7": true,
		"linux-5.1.9":  true,
		"linux-5.1.10": true,
		"pkgs":         true,
		"docs":         true,
		"org":          true,
	}
	err := filepath.Walk(env.DistriRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && ignore[info.Name()] {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(env.DistriRoot, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(builddir, "pkg/mod/distri1@v0", rel)
		if info.IsDir() {
			return os.MkdirAll(dest, 0755)
		}
		// Instead of comparing mtime, we just compare contents. This works
		// because the files are small. See https://apenwarr.ca/log/20181113
		bNew, err := ioutil.ReadFile(path)
		if err != nil {
			return err
		}
		bOld, err := ioutil.ReadFile(dest)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && bytes.Equal(bOld, bNew) {
			return nil
		}
		log.Printf("updating %s", dest)
		if err := renameio.WriteFile(dest, bNew, 0644); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return xerrors.Errorf("filepath.Walk: %v", err)
	}

	// Drop all replace directives from go.mod, if any. We only do this for the
	// distriroot:// URL scheme, because for upstream packages, go.mod should
	// not be released with replace statements pointing outside of the
	// package. If it is, that is an upstream bug and should be patched.
	type replacement struct {
		Path string
	}
	type replace struct {
		Old replacement
		New replacement
	}
	var mod struct {
		Replace []replace
	}
	gotool := exec.Command("go", "mod", "edit", "-json")
	gotool.Dir = builddir
	gotool.Stderr = os.Stderr
	out, err := gotool.Output()
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, &mod); err != nil {
		return err
	}
	for _, rep := range mod.Replace {
		if strings.HasPrefix(rep.New.Path, "./") {
			continue
		}
		log.Printf("dropping replace %s", rep.Old.Path)
		gotool := exec.Command("go", "mod", "edit", "-dropreplace", rep.Old.Path)
		gotool.Dir = builddir
		gotool.Stderr = os.Stderr
		if err := gotool.Run(); err != nil {
			return err
		}
	}

	return nil
}

func runBuildJob(job string) error {
	f := os.NewFile(uintptr(3), "")

	var b buildctx
	c, err := ioutil.ReadFile(job)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(c, &b); err != nil {
		return xerrors.Errorf("unmarshaling %q: %v", string(c), err)
	}
	c, err = ioutil.ReadFile(filepath.Join(b.PkgDir, "build.textproto"))
	if err != nil {
		return err
	}
	var buildProto pb.Build
	if err := proto.UnmarshalText(string(c), &buildProto); err != nil {
		return err
	}
	b.Proto = &buildProto

	meta, err := b.build()
	if err != nil {
		return err
	}

	{
		b, err := proto.Marshal(meta)
		if err != nil {
			return err
		}
		if _, err := f.Write(b); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}

	return nil
}

func build(args []string) error {
	//log.SetFlags(log.LstdFlags | log.Lshortfile)
	fset := flag.NewFlagSet("build", flag.ExitOnError)
	var (
		job = fset.String("job",
			"",
			"TODO")

		hermetic = fset.Bool("hermetic",
			true,
			"build hermetically (if false, host dependencies are used)")

		debug = fset.Bool("debug",
			false,
			"query to start an interactive shell during the build")

		fuse = fset.Bool("fuse",
			true,
			"Use FUSE file system instead of kernel mounts")

		ignoreGov = fset.Bool("dont_set_governor",
			false,
			"Don’t automatically set the “performance” CPU frequency scaling governor. Why wouldn’t you?")

		cross = fset.String("cross",
			"",
			"If non-empty, cross-build for the specified architecture (e.g. i686)")

		remote = fset.String("remote",
			"",
			"If non-empty, a host:port address of a remote builder")

		artifactFd = fset.Int("artifactfd",
			-1,
			"INTERNAL protocol, do not use! file descriptor number on which to print line-separated groups of NUL-separated file names of build artifacts")

		pkg = fset.String("pkg",
			"",
			"If non-empty, a package to build. Otherwise inferred from $PWD")
	)
	fset.Usage = usage(fset, buildHelp)
	fset.Parse(args)

	if *job != "" {
		return runBuildJob(*job)
	}

	if !*ignoreGov {
		cleanup, err := setGovernor("performance")
		if err != nil {
			log.Printf("Setting “performance” CPU frequency scaling governor failed: %v", err)
		} else {
			oninterrupt.Register(cleanup)
			defer cleanup()
		}
	}

	if *pkg != "" {
		if err := os.Chdir(filepath.Join(env.DistriRoot, "pkgs", *pkg)); err != nil {
			return err
		}
	}

	if _, err := os.Stat("build.textproto"); err != nil {
		if os.IsNotExist(err) {
			return xerrors.Errorf("syntax: distri build, in the pkgs/<pkg>/ directory")
		}
		return err
	}

	for _, subdir := range []string{
		"debug",
		"pkg",
		"src",
	} {
		if err := os.MkdirAll(filepath.Join("../../build/distri/", subdir), 0755); err != nil {
			return err
		}
	}

	if err := buildpkg(*hermetic, *debug, *fuse, *cross, *remote, *artifactFd); err != nil {
		return err
	}

	return nil
}
