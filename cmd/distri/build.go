package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/internal/squashfs"
	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"
	"github.com/google/renameio"
	"github.com/jacobsa/fuse"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
)

const buildHelp = `TODO
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
}

func buildpkg(hermetic, debug, fuse bool, cross string) error {
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
		Proto:    &buildProto,
		PkgDir:   pwd,
		Pkg:      filepath.Base(pwd),
		Arch:     cross,
		Version:  buildProto.GetVersion(),
		Hermetic: hermetic,
		FUSE:     fuse,
		Debug:    debug,
	}

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

	log.Printf("building %s-%s-%s", b.Pkg, b.Arch, b.Version)

	b.SourceDir, err = b.extract()
	if err != nil {
		return fmt.Errorf("extract: %v", err)
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

	{
		deps, err := b.build()
		if err != nil {
			return fmt.Errorf("build: %v", err)
		}

		// TODO: add the transitive closure of runtime dependencies

		log.Printf("runtime deps: %q", deps)

		c := proto.MarshalTextString(&pb.Meta{
			RuntimeDep: deps,
			SourcePkg:  proto.String(b.Pkg),
			Version:    proto.String(b.Version),
		})
		if err := renameio.WriteFile(filepath.Join("../distri/pkg/"+b.fullName()+".meta.textproto"), []byte(c), 0644); err != nil {
			return err
		}
	}

	// b.DestDir is /tmp/distri-dest123/tmp
	// package installs into b.DestDir/ro/hello-1

	rel := b.fullName()
	// Set fields from the perspective of an installed package so that variable
	// substitution works within wrapper scripts.
	b.Prefix = "/ro/" + rel // e.g. /ro/hello-1

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

	return nil
}

var wrapperTmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"quoteenv": func(env string) string {
		return strings.Replace(env, `=`, `="`, 1) + `"`
	},
}).Parse(`#!/ro/bin/sh
{{ range $idx, $env := .Env }}
export {{ quoteenv $env }}
{{ end }}
exec {{ .Prefix }}/{{ .Bin }} "$@"
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

func (b *buildctx) pkg() error {
	dest, err := filepath.Abs("../distri/pkg/" + b.fullName() + ".squashfs")
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

	if err := cp(w.Root, filepath.Join(filepath.Dir(b.DestDir), b.fullName())); err != nil {
		return err
	}

	if err := w.Flush(); err != nil {
		return err
	}

	if err := f.CloseAtomicallyReplace(); err != nil {
		return err
	}

	log.Printf("package successfully created in %s", dest)
	return nil
}

func (b *buildctx) substitute(s string) string {
	// TODO: different format? this might be mistaken for environment variables
	s = strings.Replace(s, "${ZI_DESTDIR}", b.DestDir, -1)
	s = strings.Replace(s, "${ZI_PREFIX}", filepath.Join(b.Prefix, "out"), -1)
	s = strings.Replace(s, "${ZI_BUILDDIR}", b.BuildDir, -1)
	s = strings.Replace(s, "${ZI_SOURCEDIR}", b.SourceDir, -1)
	return s
}

func (b *buildctx) substituteStrings(strings []string) []string {
	output := make([]string, len(strings))
	for idx, s := range strings {
		output[idx] = b.substitute(s)
	}
	return output
}

func (b *buildctx) env(deps []string, hermetic bool) []string {
	// TODO: this should go into the C builder once the C builder is used by all packages
	var (
		libDirs       []string
		pkgconfigDirs []string
		includeDirs   []string
		perl5Dirs     []string
	)
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
		if dep != "glibc-amd64-2.27" && dep != "glibc-i686-amd64-2.27" {
			includeDirs = append(includeDirs, "/ro/"+dep+"/out/include")
			includeDirs = append(includeDirs, "/ro/"+dep+"/out/include/x86_64-linux-gnu")
		}
		perl5Dirs = append(perl5Dirs, "/ro/"+dep+"/out/lib/perl5")
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
	}
	// Exclude LDFLAGS for glibc as per
	// https://github.com/Linuxbrew/legacy-linuxbrew/issues/126
	if b.Pkg != "glibc" && b.Pkg != "glibc-i686" {
		env = append(env, "LDFLAGS=-Wl,-rpath="+strings.Join(libDirs, " -Wl,-rpath=")+" -Wl,--dynamic-linker=/ro/glibc-amd64-2.27/out/lib/ld-linux-x86-64.so.2 "+strings.Join(b.Proto.GetCbuilder().GetExtraLdflag(), " ")) // for ld
	}
	return env
}

func (b *buildctx) runtimeEnv(deps []string) []string {
	// TODO: this should go into the C builder once the C builder is used by all packages
	var (
		binDirs   []string
		libDirs   []string
		perl5Dirs []string
	)
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
	}

	env := []string{
		"PATH=" + strings.Join(binDirs, ":") + ":$PATH",                       // for finding binaries
		"LD_LIBRARY_PATH=" + strings.Join(libDirs, ":") + ":$LD_LIBRARY_PATH", // for ld
		"PERL5LIB=" + strings.Join(perl5Dirs, ":") + ":$PERL5LIB",             // for perl
	}
	return env
}

var archs = map[string]bool{
	"amd64": true,
	"i686":  true,
}

func hasArchSuffix(pkg string) (suffix string, ok bool) {
	for a := range archs {
		// unversioned, but ending in an architecture already (e.g. emacs-amd64)
		if strings.HasSuffix(pkg, "-"+a) {
			return a, true
		}
	}
	return "", false
}

func (b *buildctx) glob1(imgDir, pkg string) (string, error) {
	if _, err := os.Stat(filepath.Join(imgDir, pkg+".meta.textproto")); err == nil {
		return pkg, nil // pkg already contains the version
	}
	pkgPattern := pkg
	if suffix, ok := hasArchSuffix(pkg); !ok {
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
	var meta pb.Meta
	for _, m := range matches {
		c, err := ioutil.ReadFile(m)
		if err != nil {
			return "", err
		}
		if err := proto.UnmarshalText(string(c), &meta); err != nil {
			return "", err
		}
		if meta.GetSourcePkg() != pkg {
			continue // false positive: e.g. linux-firmware-3 for pattern linux-*
		}
		candidates = append(candidates, strings.TrimSuffix(filepath.Base(m), ".meta.textproto"))
	}
	if len(candidates) > 1 {
		return "", fmt.Errorf("specify the package version to disambiguate between %q", candidates)
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("package %q not found (pattern %s)", pkg, pattern)
	}
	return candidates[0], nil
}

func (b *buildctx) glob(imgDir string, pkgs []string) ([]string, error) {
	globbed := make([]string, len(pkgs))
	for idx, pkg := range pkgs {
		var err error
		globbed[idx], err = b.glob1(imgDir, pkg)
		if err != nil {
			return nil, err
		}
	}
	return globbed, nil
}

func resolve1(imgDir string, pkg string, seen map[string]bool) ([]string, error) {
	resolved := []string{pkg}
	meta, err := readMeta(filepath.Join(imgDir, pkg+".meta.textproto"))
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
		r, err := resolve1(imgDir, dep, seen)
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
func resolve(imgDir string, pkgs []string) ([]string, error) {
	var resolved []string
	seen := make(map[string]bool)
	for _, pkg := range pkgs {
		if seen[pkg] {
			continue // a recursive call might have found this package already
		}
		seen[pkg] = true
		r, err := resolve1(imgDir, pkg, seen)
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

			"patchelf", // for shrinking the RPATH

			"strace", // useful for interactive debugging
		}

		// TODO: check for native
		if b.Arch == "amd64" {
			nativeDeps = append(nativeDeps, "gcc", "binutils")
		} else {
			nativeDeps = append(nativeDeps,
				"gcc-"+b.Arch,
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

		case *pb.Build_Cbuilder:
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
	return resolve(env.DefaultRepo, deps)
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

func (b *buildctx) build() (runtimedeps []string, _ error) {
	if os.Getenv("ZI_BUILD_PROCESS") != "1" {
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
			return nil, fmt.Errorf("builddeps: %v", err)
		}

		if b.FUSE {
			if _, err = mountfuse([]string{"-overlays=/bin,/out/lib/pkgconfig,/out/include,/out/include/scsi,/out/include/sys,/out/include/gnu", "-pkgs=" + strings.Join(deps, ","), depsdir}); err != nil {
				return nil, err
			}
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
		cmd.Env = append(os.Environ(), "ZI_BUILD_PROCESS=1")
		cmd.Stdin = os.Stdin // for interactive debugging
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("%v: %v", cmd.Args, err)
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
		deps = append(meta.GetRuntimeDep(), b.Proto.GetRuntimeDep()...)

		deps, err = b.glob(env.DefaultRepo, deps)
		if err != nil {
			return nil, err
		}

		resolved, err := resolve(env.DefaultRepo, deps)
		if err != nil {
			return nil, err
		}
		return resolved, cmd.Wait()
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

	// TODO: link /bin to /ro/bin, then set PATH=/ro/bin

	// The hermetic build environment contains the following paths:
	//  /bin/sh → /ro/bin/bash (scripts expect /bin/sh to be present)
	//  /dev/null
	//	/dest/<destdir>
	//	/ro/<deps>
	//  /ro/bin (PATH=/ro/bin:/bin)
	//	/usr/src/<srcdir>
	//  /tmp/<builddir>

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
				return nil, fmt.Errorf("bind mount %s %s: %v", b.SourceDir, src, err)
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
			prefix := filepath.Join(b.ChrootDir, "ro", b.fullName())
			b.Prefix = strings.TrimPrefix(prefix, b.ChrootDir)

			// Make available b.DestDir as /dest/tmp:
			dst := filepath.Join(b.ChrootDir, "dest", "tmp")
			if err := os.MkdirAll(dst, 0755); err != nil {
				return nil, err
			}
			if err := syscall.Mount(b.DestDir, dst, "none", syscall.MS_BIND, ""); err != nil {
				return nil, fmt.Errorf("bind mount %s %s: %v", b.DestDir, dst, err)
			}
			b.DestDir = strings.TrimPrefix(dst, b.ChrootDir)

			if _, err := os.Stat(prefix); os.IsNotExist(err) {
				// Bind /dest/tmp to prefix (e.g. /ro/systemd-amd64-239) so that
				// shlibdeps works for binaries which depend on libraries they
				// install.
				if err := fuseMkdirAll(filepath.Join(b.ChrootDir, "ro", "ctl"), b.fullName()); err != nil {
					return nil, err
				}
				if err := syscall.Mount(dst, prefix, "none", syscall.MS_BIND, ""); err != nil {
					return nil, fmt.Errorf("bind mount %s %s: %v", dst, prefix, err)
				}
			}

			// Symlinks:
			//   /bin → /ro/bin
			//   /usr/bin → /ro/bin (for e.g. /usr/bin/env)
			//   /sbin → /ro/bin (for e.g. linux, which hard-codes /sbin/depmod)
			//   /lib64 → /ro/glibc-amd64-2.27/out/lib for ld-linux-x86-64.so.2
			//   /lib → /ro/glibc-i686-amd64-2.27/out/lib for ld-linux.so.2

			// TODO: glob glibc? chose newest? error on >1 glibc?
			// TODO: do we still need this for native builds?
			if err := os.Symlink("/ro/glibc-amd64-2.27/out/lib", filepath.Join(b.ChrootDir, "lib64")); err != nil {
				return nil, err
			}

			// TODO: test for cross
			if b.Arch != "amd64" {
				// gcc-i686 and binutils-i686 are built with --sysroot=/,
				// meaning they will search for startup files (e.g. crt1.o) in
				// $(sysroot)/lib.
				// TODO: try compiling with --sysroot pointing to /ro/glibc-i686-amd64-2.27/out/lib directly?
				if err := os.Symlink("/ro/glibc-i686-amd64-2.27/out/lib", filepath.Join(b.ChrootDir, "lib")); err != nil {
					return nil, err
				}
			}

			if !b.FUSE {
				if err := os.Symlink("/ro/glibc-amd64-2.27/out/lib", filepath.Join(b.ChrootDir, "ro", "lib")); err != nil {
					return nil, err
				}
			} else {
				if err := os.Symlink("/ro/include", filepath.Join(b.ChrootDir, "usr", "include")); err != nil {
					return nil, err
				}
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
				return nil, fmt.Errorf("mount tmpfs /usr/src: %v", err)
			}
			if err := os.MkdirAll(src, 0755); err != nil {
				return nil, err
			}
			if err := syscall.Mount(b.SourceDir, src, "none", syscall.MS_BIND|syscall.MS_RDONLY, ""); err != nil {
				return nil, fmt.Errorf("bind mount %s %s: %v", b.SourceDir, src, err)
			}
			b.SourceDir = src
		}

		{
			// Make available b.DestDir as /ro/<pkg>-<version>:
			dst := filepath.Join("/ro", "tmp")
			// TODO: get rid of the requirement of having (an empty) /ro exist on the host
			if err := syscall.Mount("tmpfs", "/ro", "tmpfs", 0, ""); err != nil {
				return nil, fmt.Errorf("mount tmpfs /ro: %v", err)
			}
			if err := os.MkdirAll(dst, 0755); err != nil {
				return nil, err
			}
			if err := syscall.Mount(b.DestDir, dst, "none", syscall.MS_BIND, ""); err != nil {
				return nil, fmt.Errorf("bind mount %s %s: %v", b.DestDir, dst, err)
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
			if err := install(append([]string{"-root=/ro"}, deps...)); err != nil {
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
			// 	return fmt.Errorf("bind mount %s %s: %v", "/ro/bin", "/bin", err)
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
		case *pb.Build_Perlbuilder:
			var err error
			steps, env, err = b.buildperl(v.Perlbuilder, env)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("BUG: unknown builder")
		}
	}

	if len(steps) == 0 {
		return nil, fmt.Errorf("build.textproto does not specify Builder nor BuildSteps")
	}

	if b.Hermetic {
		log.Printf("build environment variables:")
		for _, kv := range env {
			log.Printf("  %s", kv)
		}
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

	for _, unit := range b.Proto.GetInstall().GetSystemdUnit() {
		fn := b.substitute(unit)
		if _, err := os.Stat(fn); err != nil {
			return nil, fmt.Errorf("unit %q: %v", unit, err)
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

				if err := ioutil.WriteFile(newname, buf.Bytes(), 0755); err != nil {
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
		pkgs, err := findShlibDeps(path)
		if err != nil {
			if err == errLddFailed {
				return nil // skip patchelf
			}
			return err
		}
		for _, pkg := range pkgs {
			depPkgs[pkg] = true
		}

		// TODO: make patchelf able to operate on itself
		if b.Pkg != "patchelf" &&
			filepath.Base(path) != "Mcrt1.o" &&
			filepath.Base(path) != "Scrt1.o" &&
			filepath.Base(path) != "crti.o" &&
			filepath.Base(path) != "crtn.o" &&
			filepath.Base(path) != "gcrt1.o" &&
			filepath.Base(path) != "crt1.o" &&
			!strings.HasSuffix(path, ".a") {
			patchelf := exec.Command("patchelf", "--shrink-rpath", path)
			patchelf.Stdout = os.Stdout
			patchelf.Stderr = os.Stderr
			if err := patchelf.Run(); err != nil {
				return fmt.Errorf("%v: %v", patchelf.Args, err)
			}
		}
		return nil
	})
	if err != nil {
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
			if strings.HasPrefix(line, "Requires.private: ") ||
				strings.HasPrefix(line, "Requires: ") {
				// TODO: add packages which contain this pkgconfig file
				log.Printf("TODO: extract names from %q", line)
			}
		}
	}

	if builder := b.Proto.Builder; builder != nil {
		switch builder.(type) {
		case *pb.Build_Cbuilder:
		case *pb.Build_Perlbuilder:
			depPkgs["perl-amd64-5.28.0"] = true
			// pass through all deps to run-time deps
			// TODO: distinguish test-only deps from actual deps based on Makefile.PL
			for _, pkg := range b.Proto.GetDep() {
				depPkgs[pkg] = true
			}
		default:
			return nil, fmt.Errorf("BUG: unknown builder")
		}
	}

	// prevent circular runtime dependencies
	delete(depPkgs, b.Pkg)
	delete(depPkgs, b.fullName())

	log.Printf("run-time dependencies: %+v", depPkgs)
	deps = make([]string, 0, len(depPkgs))
	for pkg := range depPkgs {
		deps = append(deps, pkg)
	}

	return deps, nil
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
			return fmt.Errorf("%v: %v", cmd.Args, err)
		}
		return nil
	}
	// TODO: remove the URL support. we want patches to be committed alongside the packaging
	resp, err := http.Get(src)
	if err != nil {
		return err
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		return fmt.Errorf("HTTP status %v", resp.Status)
	}
	// TODO: once we extract in golang tar, we can just re-extract the timestamps
	cmd := exec.Command("patch", "-p1", "--batch", "--set-time", "--set-utc")
	cmd.Dir = tmp
	cmd.Stdin = resp.Body
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %v", cmd.Args, err)
	}
	return nil
}

func (b *buildctx) extract() (srcdir string, _ error) {
	fn := filepath.Base(b.Proto.GetSource())
	dir := fn
	for _, suffix := range []string{"gz", "lz", "xz", "bz2", "tar", "tgz"} {
		dir = strings.TrimSuffix(dir, "."+suffix)
	}
	_, err := os.Stat(dir)
	if err == nil {
		return dir, nil // already extracted
	}

	if !os.IsNotExist(err) {
		return "", err // directory exists, but can’t access it?
	}

	if err := b.verify(fn); err != nil {
		return "", fmt.Errorf("verify: %v", err)
	}

	pwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	tmp, err := ioutil.TempDir(pwd, "distri")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	// TODO(later): extract in pure Go to avoid tar dependency
	cmd := exec.Command("tar", "xf", fn, "--strip-components=1", "--no-same-owner", "-C", tmp)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}

	for _, u := range b.Proto.GetCherryPick() {
		if err := b.cherryPick(u, tmp); err != nil {
			return "", fmt.Errorf("cherry picking %s: %v", u, err)
		}
		log.Printf("cherry picked %s", u)
	}

	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}

	return dir, nil
}

func (b *buildctx) verify(fn string) error {
	_, err := os.Stat(fn)
	if err != nil {
		if !os.IsNotExist(err) {
			return err // file exists, but can’t access it?
		}

		// TODO(later): calculate hash while downloading to avoid having to read the file
		if err := b.download(fn); err != nil {
			return err
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
		return fmt.Errorf("hash mismatch for %s: got %s, want %s", fn, got, want)
	}
	return nil
}

func (b *buildctx) download(fn string) error {
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
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		return fmt.Errorf("unexpected HTTP status: got %d (%v), want %d", got, resp.Status, want)
	}
	f, err := os.Create(fn)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	resp.Body.Close()
	return f.Close()
}

func runJob(job string) error {
	f := os.NewFile(uintptr(3), "")

	var b buildctx
	c, err := ioutil.ReadFile(job)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(c, &b); err != nil {
		return fmt.Errorf("unmarshaling %q: %v", string(c), err)
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

	deps, err := b.build()
	if err != nil {
		return err
	}

	{
		b, err := proto.Marshal(&pb.Meta{
			RuntimeDep: deps,
		})
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
	)
	fset.Parse(args)

	if *job != "" {
		return runJob(*job)
	}

	if !*ignoreGov {
		cleanup, err := setGovernor("performance")
		if err != nil {
			log.Printf("Setting “performance” CPU frequency scaling governor failed: %v", err)
		} else {
			onInterruptMu.Lock()
			onInterrupt = append(onInterrupt, cleanup)
			onInterruptMu.Unlock()
			defer cleanup()
		}
	}

	if _, err := os.Stat("build.textproto"); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("syntax: distri build, in the pkg/<pkg>/ directory")
		}
		return err
	}

	if err := buildpkg(*hermetic, *debug, *fuse, *cross); err != nil {
		return err
	}

	return nil
}
