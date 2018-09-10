package main

import (
	"bytes"
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
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/stapelberg/zi/internal/squashfs"
	"github.com/stapelberg/zi/pb"
	"golang.org/x/sys/unix"
)

var (
	// TODO: make -job a flag of the build verb
	job = flag.String("job",
		"",
		"TODO")

	// TODO: make hermetic a build flag
	hermetic = flag.Bool("hermetic",
		true,
		"build hermetically (if false, host dependencies are used)")

	// TODO: make debug a build flag
	debug = flag.Bool("debug",
		false,
		"query to start an interactive shell during the build")
)

type buildctx struct {
	Proto     *pb.Build `json:"-"`
	PkgDir    string    // e.g. /home/michael/zi/pkgs/busybox
	Pkg       string    // e.g. busybox
	Version   string    // e.g. 1.29.2
	SourceDir string    // e.g. /home/michael/zi/build/busybox/busybox-1.29.2
	BuildDir  string    // e.g. /tmp/zibuild-8123911
	DestDir   string    // e.g. /tmp/zidest-3129384
	Prefix    string    // e.g. /ro/busybox-1.29.2
	Hermetic  bool
	Debug     bool
	ChrootDir string // only set if Hermetic is enabled
}

func buildpkg() error {
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

	b := &buildctx{
		Proto:    &buildProto,
		PkgDir:   pwd,
		Pkg:      filepath.Base(pwd),
		Version:  buildProto.GetVersion(),
		Hermetic: *hermetic,
		Debug:    *debug,
	}

	{
		tmpdir, err := ioutil.TempDir("", "zi-dest")
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

	log.Printf("building %s-%s", b.Pkg, b.Version)

	b.SourceDir, err = b.extract()
	if err != nil {
		return fmt.Errorf("extract: %v", err)
	}

	b.SourceDir, err = filepath.Abs(b.SourceDir)
	if err != nil {
		return err
	}

	{
		tmpdir, err := ioutil.TempDir("", "zi-build")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmpdir)
		b.BuildDir = tmpdir
	}

	if err := os.MkdirAll(filepath.Join(b.DestDir, "buildoutput"), 0755); err != nil {
		return err
	}

	{
		deps, err := b.build()
		if err != nil {
			return err
		}

		// TODO: add the transitive closure of runtime dependencies

		log.Printf("runtime deps: %q", deps)

		c := proto.MarshalTextString(&pb.Meta{
			RuntimeDep: deps,
		})
		if err := ioutil.WriteFile(filepath.Join("../zi/pkg/"+b.Pkg+"-"+b.Version+".meta.textproto"), []byte(c), 0644); err != nil {
			return err
		}
	}

	// b.DestDir is /tmp/zi-dest123/tmp
	// package installs into b.DestDir/ro/hello-1

	rel := b.Pkg + "-" + b.Version

	destDir := filepath.Join(filepath.Dir(b.DestDir), rel)

	// rename b.DestDir/ro/hello-1 to b.DestDir/../hello-1:
	if err := os.Rename(filepath.Join(b.DestDir, "ro", rel), destDir); err != nil {
		return err
	}

	// TODO: create binary wrappers for runtime deps (symlinks for now)
	if err := os.MkdirAll(filepath.Join(destDir, "bin"), 0755); err != nil {
		return err
	}
	for _, dir := range []string{"bin", "sbin"} {
		dir = filepath.Join(destDir, "buildoutput", dir)
		// TODO(performance): read directories directly, don’t care about sorting
		fis, err := ioutil.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		for _, fi := range fis {
			oldname := filepath.Join(dir, fi.Name())
			newname := filepath.Join(destDir, "bin", fi.Name())
			oldname, err = filepath.Rel(filepath.Join(destDir, "bin"), oldname)
			if err != nil {
				return err
			}
			if err := os.Symlink(oldname, newname); err != nil {
				return err
			}
		}
	}

	if err := b.pkg(); err != nil {
		return err
	}

	return nil
}

func (b *buildctx) serialize() (string, error) {
	// TODO: exempt the proto field from marshaling, it needs jsonpb once you use oneofs
	enc, err := json.Marshal(b)
	if err != nil {
		return "", err
	}

	tmp, err := ioutil.TempFile("", "zi")
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
	// TODO: create the archive in pure Go
	dest, err := filepath.Abs("../zi/pkg/" + b.Pkg + "-" + b.Version + ".squashfs")
	if err != nil {
		return err
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	w, err := squashfs.NewWriter(f, time.Now())
	if err != nil {
		return err
	}

	if err := cp(w.Root, filepath.Join(filepath.Dir(b.DestDir), b.Pkg+"-"+b.Version)); err != nil {
		return err
	}

	if err := w.Flush(); err != nil {
		return err
	}

	if err := f.Close(); err != nil {
		return err
	}

	// rel := b.Pkg + "-" + b.Version
	// cmd := exec.Command("tar", "czf", dest, rel)
	// cmd.Dir = filepath.Dir(b.DestDir)
	// cmd.Stderr = os.Stderr
	// if err := cmd.Run(); err != nil {
	// 	return fmt.Errorf("%v: %v", cmd.Args, err)
	// }

	log.Printf("package successfully created in %s", dest)
	return nil
}

func (b *buildctx) substitute(s string) string {
	// TODO: different format? this might be mistaken for environment variables
	s = strings.Replace(s, "${ZI_DESTDIR}", b.DestDir, -1)
	s = strings.Replace(s, "${ZI_PREFIX}", filepath.Join(b.Prefix, "buildoutput"), -1)
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

func builddeps(p *pb.Build) []string {
	deps := p.GetDep()
	if builder := p.Builder; builder != nil {
		switch builder.(type) {
		case *pb.Build_Cbuilder:
			// configure runtime dependencies:
			deps = append(deps, []string{
				"bash-4.4.18",
				"coreutils-8.30",
				"sed-4.5",
				"grep-3.1",
				"gawk-4.2.1",
				"diffutils-3.6",
				"file-5.34",
			}...)

			// C build environment:
			deps = append(deps, []string{
				"gcc-8.2.0",
				"mpc-1.1.0",  // TODO: remove once gcc binaries find these via their rpath
				"mpfr-4.0.1", // TODO: remove once gcc binaries find these via their rpath
				"gmp-6.1.2",  // TODO: remove once gcc binaries find these via their rpath
				"binutils-2.31",
				"make-4.2.1",
				"glibc-2.27",
				"linux-4.18.7",
				"findutils-4.6.0", // find(1) is used by libtool, build of e.g. libidn2 will fail if not present
			}...)
		}
	}

	return deps
}

func (b *buildctx) build() (runtimedeps []string, _ error) {
	if os.Getenv("ZI_BUILD_PROCESS") != "1" {
		chrootDir, err := ioutil.TempDir("", "zi-buildchroot")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(chrootDir)
		b.ChrootDir = chrootDir

		// Install build dependencies into /ro
		deps := filepath.Join(b.ChrootDir, "ro")
		// TODO: mount() does this, no?
		if err := os.MkdirAll(deps, 0755); err != nil {
			return nil, err
		}

		for _, dep := range builddeps(b.Proto) {
			cleanup, err := mount([]string{"-root=" + deps, dep})
			if err != nil {
				return nil, err
			}
			defer cleanup()
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
			os.Args[0], "-job="+serialized)
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
		b, err := ioutil.ReadAll(r)
		if err != nil {
			return nil, err
		}
		var meta pb.Meta
		if err := proto.Unmarshal(b, &meta); err != nil {
			return nil, err
		}
		return meta.GetRuntimeDep(), nil
	}

	logDir := filepath.Dir(b.SourceDir) // TODO: introduce a struct field
	buildLog, err := os.Create(filepath.Join(logDir, "build-"+b.Version+".log"))
	if err != nil {
		return nil, err
	}
	defer buildLog.Close()

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
			src := filepath.Join(b.ChrootDir, "usr", "src", b.Pkg+"-"+b.Version)
			if err := os.MkdirAll(src, 0755); err != nil {
				return nil, err
			}
			if err := syscall.Mount(b.SourceDir, src, "none", syscall.MS_BIND|syscall.MS_RDONLY, ""); err != nil {
				return nil, fmt.Errorf("bind mount %s %s: %v", b.SourceDir, src, err)
			}
			b.SourceDir = strings.TrimPrefix(src, b.ChrootDir)
		}

		{
			// Make available b.DestDir as /dest/<pkg>-<version>:
			prefix := filepath.Join(b.ChrootDir, "ro", b.Pkg+"-"+b.Version)
			if err := os.MkdirAll(prefix, 0755); err != nil {
				return nil, err
			}
			b.Prefix = strings.TrimPrefix(prefix, b.ChrootDir)

			dst := filepath.Join(b.ChrootDir, "dest", "tmp")
			if err := os.MkdirAll(dst, 0755); err != nil {
				return nil, err
			}
			if err := syscall.Mount(b.DestDir, dst, "none", syscall.MS_BIND, ""); err != nil {
				return nil, fmt.Errorf("bind mount %s %s: %v", b.DestDir, dst, err)
			}
			b.DestDir = strings.TrimPrefix(dst, b.ChrootDir)

			// // Install build dependencies into /ro
			// deps := filepath.Join(b.ChrootDir, "ro")
			// if err := os.MkdirAll(deps, 0755); err != nil {
			// 	return nil, err
			// }
			// flag.Set("root", deps) // TODO: make install() take an arg

			// // TODO: the builder should likely install dependencies as required
			// // (e.g. if autotools is detected, bash+coreutils+sed+grep+gawk need to
			// // be installed as runtime env, and gcc+binutils+make for building)

			// if err := install(b.Proto.GetDep()); err != nil {
			// 	return nil, err
			// }

			deps := filepath.Join(b.ChrootDir, "ro")

			// Symlinks:
			//   /ro/bin/sh → bash
			//   /bin → /ro/bin
			//   /usr/bin → /ro/bin (for e.g. /usr/bin/env)
			//   /sbin → /ro/bin (for e.g. linux, which hard-codes /sbin/depmod)
			//   /lib64 → /ro/glibc-2.27/buildoutput/lib for ld-linux.so
			if err := os.Symlink("bash", filepath.Join(deps, "bin", "sh")); err != nil {
				return nil, err
			}
			// TODO: glob glibc? chose newest? error on >1 glibc?
			if err := os.Symlink("/ro/glibc-2.27/buildoutput/lib", filepath.Join(b.ChrootDir, "lib64")); err != nil {
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
			src := filepath.Join("/usr/src", b.Pkg+"-"+b.Version)
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

			prefix := filepath.Join("/ro", b.Pkg+"-"+b.Version)
			if err := os.MkdirAll(prefix, 0755); err != nil {
				return nil, err
			}
			b.Prefix = prefix

			// Install build dependencies into /ro

			// TODO: the builder should likely install dependencies as required
			// (e.g. if autotools is detected, bash+coreutils+sed+grep+gawk need to
			// be installed as runtime env, and gcc+binutils+make for building)

			if err := install(append([]string{"-root=/ro"}, builddeps(b.Proto)...)); err != nil {
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

	// TODO: this should go into the C builder once the C builder is used by all packages
	var (
		libDirs       []string
		pkgconfigDirs []string
		includeDirs   []string
	)
	// add the package itself, not just its dependencies: the package might
	// install a shared library which it also uses (e.g. systemd).
	for _, dep := range append(builddeps(b.Proto), b.Pkg+"-"+b.Version) {
		libDirs = append(libDirs, "/ro/"+dep+"/buildoutput/lib")
		// TODO: should we try to make programs install to /lib instead? examples: libffi
		libDirs = append(libDirs, "/ro/"+dep+"/buildoutput/lib64")
		pkgconfigDirs = append(pkgconfigDirs, "/ro/"+dep+"/buildoutput/lib/pkgconfig")
		includeDirs = append(includeDirs, "/ro/"+dep+"/buildoutput/include")
		includeDirs = append(includeDirs, "/ro/"+dep+"/buildoutput/include/x86_64-linux-gnu")
	}

	env := []string{
		// TODO: remove /ro/bin hack for python, file bug: python3 -c 'import sys;print(sys.path)' prints wrong result with PATH=/bin and /bin→/ro/bin and /ro/bin/python3→../python3-3.7.0/bin/python3
		"PATH=/ro/bin:bin",                                            // for finding binaries
		"LIBRARY_PATH=" + strings.Join(libDirs, ":"),                  // for gcc
		"LD_LIBRARY_PATH=" + strings.Join(libDirs, ":"),               // for ld
		"CPATH=" + strings.Join(includeDirs, ":"),                     // for gcc
		"LDFLAGS=-Wl,-rpath=" + strings.Join(libDirs, " -Wl,-rpath="), // for ld
		"PKG_CONFIG_PATH=" + strings.Join(pkgconfigDirs, ":"),         // for pkg-config
	}

	steps := b.Proto.GetBuildStep()
	if builder := b.Proto.Builder; builder != nil && len(steps) == 0 {
		switch v := builder.(type) {
		case *pb.Build_Cbuilder:
			if err := b.buildc(v.Cbuilder, env, buildLog); err != nil {
				return nil, err
			}
		}
	} else {
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
				log.Printf("build step failed (%v), starting debug shell", err)
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
		dest := filepath.Join(b.DestDir, b.Prefix, "buildoutput", "lib", "systemd", "system")
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
		dest := filepath.Join(b.DestDir, b.Prefix, "buildoutput")
		if err := os.Symlink(oldname, filepath.Join(dest, newname)); err != nil {
			return nil, err
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
		// TODO: store deps
		pkgs, err := findShlibDeps(path)
		if err != nil {
			return err
		}
		for _, pkg := range pkgs {
			depPkgs[pkg] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	log.Printf("run-time dependencies: %+v", depPkgs)
	deps := make([]string, 0, len(depPkgs))
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
	for _, suffix := range []string{"gz", "lz", "xz", "bz2", "tar"} {
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
	tmp, err := ioutil.TempDir(pwd, "zi")
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
	log.Printf("downloading %s to %s", b.Proto.GetSource(), fn)
	resp, err := http.Get(b.Proto.GetSource())
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

func main() {
	flag.Parse()

	if os.Getpid() == 1 {
		if err := pid1(); err != nil {
			log.Fatal(err)
		}
	}

	if *job != "" {
		if err := runJob(*job); err != nil {
			log.Fatal(err)
		}
		return
	}

	args := flag.Args()
	verb := "build" // TODO: change default to install
	if len(args) > 0 {
		verb, args = args[0], args[1:]
	}

	switch verb {
	case "build":
		if _, err := os.Stat("build.textproto"); err != nil {
			if os.IsNotExist(err) {
				log.Fatalf("syntax: zi build, in the pkg/<pkg>/ directory")
			}
			log.Fatal(err)
		}

		if err := buildpkg(); err != nil {
			log.Fatal(err)
		}

		// TODO: remove this once we build to SquashFS by default
	case "convert":
		if err := convert(args); err != nil {
			log.Fatal(err)
		}

	case "mount":
		if _, err := mount(args); err != nil {
			log.Fatal(err)
		}

	case "umount":
		if err := umount(args); err != nil {
			log.Fatal(err)
		}

	case "ninja":
		if err := ninja(args); err != nil {
			log.Fatal(err)
		}

	case "pack":
		if err := pack(args); err != nil {
			log.Fatal(err)
		}

	case "install":
		if err := install(args); err != nil {
			log.Fatal(err)
		}
	}
}
