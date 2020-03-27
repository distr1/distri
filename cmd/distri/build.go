package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/build"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/internal/trace"
	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"
	"github.com/google/renameio"
	"golang.org/x/xerrors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/prototext"

	bpb "github.com/distr1/distri/pb/builder"
)

const buildHelp = `distri build [-flags]

Build a distri package.

Example:
  % distri build -pkg=i3status
`

const (
	tidBuildpkg = iota
	tidSquashfsSrc
)

func updateFromDistriroot(builddir string) error {
	// TODO(later): fill ignore from .gitignore?
	ignore := map[string]bool{
		".git":         true,
		"linux-4.18.7": true,
		"linux-5.1.9":  true,
		"linux-5.1.10": true,
		"pkgs":         true,
		"docs":         true,
		"org":          true,
	}
	ignoreRel := map[string]bool{
		"build": true,
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
		if ignoreRel[rel] {
			return filepath.SkipDir
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

func buildpkg(ctx context.Context, hermetic bool, debug string, fuse bool, pwd, cross, remote string, artifactFd, jobs int) error {
	defer trace.Event("buildpkg", tidBuildpkg).Done()
	buildProto, err := pb.ReadBuildFile("build.textproto")
	if err != nil {
		return err
	}

	if cross == "" {
		cross = "amd64" // TODO: configurable / auto-detect
	}

	b := &build.Ctx{
		Repo:           env.DefaultRepo,
		Proto:          buildProto,
		PkgDir:         pwd,
		Pkg:            filepath.Base(pwd),
		Arch:           cross,
		Version:        buildProto.GetVersion(),
		Hermetic:       hermetic,
		FUSE:           fuse,
		Debug:          debug,
		ArtifactWriter: ioutil.Discard,
		Jobs:           jobs,
	}

	if artifactFd > -1 {
		b.ArtifactWriter = os.NewFile(uintptr(artifactFd), "")
	}

	// Set fields from the perspective of an installed package so that variable
	// substitution works within wrapper scripts.
	b.Prefix = "/ro/" + b.FullName() // e.g. /ro/hello-amd64-1

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

	buildLog, err := os.Create("build-" + b.Version + ".log")
	if err != nil {
		return err
	}
	defer buildLog.Close()
	multiLog := io.MultiWriter(os.Stderr, buildLog)
	log.SetOutput(multiLog)

	log.Printf("building %s", b.FullName())

	b.SourceDir = build.TrimArchiveSuffix(filepath.Base(b.Proto.GetSource()))

	u, err := url.Parse(b.Proto.GetSource())
	if err != nil {
		return xerrors.Errorf("url.Parse: %v", err)
	}

	extractEv := trace.Event("extract", tidBuildpkg)
	if u.Scheme == "distriroot" {
		if err := updateFromDistriroot(b.SourceDir); err != nil {
			return xerrors.Errorf("updateFromDistriroot: %v", err)
		}
	} else if u.Scheme == "empty" {
		b.SourceDir = "empty"
		if err := b.MakeEmpty(); err != nil {
			return xerrors.Errorf("makeEmpty: %v", err)
		}
	} else if u.Scheme == "distri+source" {
		redirected := b.Clone()
		redirected.Pkg = u.Host
		if err := os.Chdir("../" + redirected.Pkg); err != nil {
			return err
		}
		redirected.PkgDir = filepath.Join(env.DistriRoot, "pkgs", redirected.Pkg)
		bld, err := pb.ReadBuildFile(filepath.Join(redirected.PkgDir, "build.textproto"))
		if err != nil {
			return err
		}
		redirected.Proto = bld
		redirected.SourceDir = build.TrimArchiveSuffix(filepath.Base(bld.GetSource()))
		b.SourceDir = redirected.SourceDir
		log.Printf("redirected.SourceDir=%s", redirected.SourceDir)
		if err := redirected.Extract(); err != nil {
			return xerrors.Errorf("extract: %v", err)
		}
	} else {
		if err := b.Extract(); err != nil {
			return xerrors.Errorf("extract: %v", err)
		}
	}
	extractEv.Done()

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

	// Call digest() for the side-effect of populating the b.Digest field, which
	// will then be available in the child process, too.
	b.Digest()

	if remote != "" {
		defer trace.Event("remote", tidBuildpkg).Done()

		log.Printf("building on %s", remote)

		conn, err := grpc.DialContext(ctx, remote, grpc.WithInsecure(), grpc.WithBlock())
		if err != nil {
			return err
		}
		cl := bpb.NewBuildClient(conn)

		deps, err := b.Builddeps(b.Proto)
		if err != nil {
			return xerrors.Errorf("builddeps: %v", err)
		}

		alldeps := make([]string, len(deps))
		copy(alldeps, deps)
		alldeps = append(alldeps, b.Proto.GetRuntimeDep()...)
		resolved, err := b.GlobAndResolve(env.DefaultRepo, alldeps, "")
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
			storeEv := trace.Event("store "+input, tidBuildpkg)
			log.Printf("store(%s)", input)
			err := store(ctx, cl, input)
			storeEv.Done()
			if err != nil {
				return err
			}
		}

		var artifacts []string
		buildEv := trace.Event("build "+b.Pkg, tidBuildpkg)
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
		buildEv.Done()

		for _, fn := range artifacts {
			retrieveEv := trace.Event("retrieve "+fn, tidBuildpkg)
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
			retrieveEv.Done()
		}

		return nil
	}

	buildEv := trace.Event("build "+b.Pkg, tidBuildpkg)
	meta, err := b.Build(ctx, buildLog)
	if err != nil {
		return xerrors.Errorf("build: %v", err)
	}
	buildEv.Done()

	capsEv := trace.Event("setcaps", tidBuildpkg)
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

	capsEv.Done()

	// b.DestDir is /tmp/distri-dest123/tmp
	// package installs into b.DestDir/ro/hello-1

	rel := b.FullName()

	destDir := filepath.Join(filepath.Dir(b.DestDir), rel) // e.g. /tmp/distri-dest123/hello-1

	// rename destDir/tmp/ro/hello-1 to destDir/hello-1:
	if err := os.Rename(filepath.Join(b.DestDir, "ro", rel), destDir); err != nil {
		return err
	}

	// rename destDir/tmp/etc to destDir/etc
	if err := os.Rename(filepath.Join(b.DestDir, "etc"), filepath.Join(destDir, "etc")); err != nil && !os.IsNotExist(err) {
		return err
	}

	if err := b.Package(); err != nil {
		return err
	}

	pkgs := append(b.Proto.GetSplitPackage(), &pb.SplitPackage{
		Name:  proto.String(b.Pkg),
		Claim: []*pb.Claim{{Glob: proto.String("*")}},
	})
	for _, pkg := range pkgs {
		fullName := pkg.GetName() + "-" + b.Arch + "-" + b.Version
		writeEv := trace.Event("write "+fullName, tidBuildpkg)

		deps := append(meta.GetRuntimeDep(),
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
		resolved, err := b.GlobAndResolve(env.DefaultRepo, deps, pkg.GetName())
		if err != nil {
			return fmt.Errorf("resolve: %w", err)
		}

		// TODO: add the transitive closure of runtime dependencies

		log.Printf("%s runtime deps: %q", pkg.GetName(), resolved)

		unions := make([]*pb.Union, len(b.Proto.RuntimeUnion))
		for idx, o := range b.Proto.RuntimeUnion {
			globbed, err := b.Glob1(env.DefaultRepo, o.GetPkg())
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
			InputDigest:  proto.String(b.InputDigest),
		})
		fn := filepath.Join("../distri/pkg/" + fullName + ".meta.textproto")
		b.ArtifactWriter.Write([]byte("build/" + strings.TrimPrefix(fn, "../") + "\n"))
		if err := renameio.WriteFile(fn, []byte(c), 0644); err != nil {
			return err
		}
		if err := renameio.Symlink(fullName+".meta.textproto", filepath.Join("../distri/pkg/"+pkg.GetName()+"-"+b.Arch+".meta.textproto")); err != nil {
			return err
		}
		writeEv.Done()
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

func runBuildJob(ctx context.Context, job string) error {
	f := os.NewFile(uintptr(3), "")

	var b build.Ctx
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
	if err := prototext.Unmarshal(c, &buildProto); err != nil {
		return err
	}
	b.Proto = &buildProto

	meta, err := b.Build(ctx, ioutil.Discard)
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

func cmdbuild(ctx context.Context, args []string) error {
	//log.SetFlags(log.LstdFlags | log.Lshortfile)
	fset := flag.NewFlagSet("build", flag.ExitOnError)
	var (
		job = fset.String("job",
			"",
			"TODO")

		hermetic = fset.Bool("hermetic",
			true,
			"build hermetically (if false, host dependencies are used)")

		debug = fset.String("debug",
			"",
			"if non-empty, start an interactive shell at the specified stage in the build. one of after-steps, after-install, after-wrapper, after-loopmount, after-elf, after-libfarm")

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

		jobs = fset.Int("jobs",
			runtime.NumCPU(),
			"Number of parallel jobs, passed to make -j, ninja --jobs, etc.")
	)
	fset.Usage = usage(fset, buildHelp)
	fset.Parse(args)

	if *job != "" {
		return runBuildJob(ctx, *job)
	}

	var pwd string
	if *pkg != "" {
		pwd = filepath.Join(env.DistriRoot, "pkgs", *pkg)
		if err := os.Chdir(pwd); err != nil {
			return err
		}
	} else {
		var err error
		pwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}

	if *ctracefile == "" {
		// Enable writing ctrace output files by default for distri build. Not
		// specifying the flag is a time- and power-costly mistake :)
		trace.Enable("build." + filepath.Base(pwd))
		const freq = 1 * time.Second
		go trace.CPUEvents(ctx, freq)
		go trace.MemEvents(ctx, freq)
	}

	if !*ignoreGov {
		cleanup, err := setGovernor("performance")
		if err != nil {
			log.Printf("Setting “performance” CPU frequency scaling governor failed: %v", err)
		} else {
			defer cleanup()
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

	if err := buildpkg(ctx, *hermetic, *debug, *fuse, pwd, *cross, *remote, *artifactFd, *jobs); err != nil {
		return err
	}

	return nil
}
