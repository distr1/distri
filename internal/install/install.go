package install

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	// TODO: consider "github.com/klauspost/pgzip"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/internal/repo"
	"github.com/distr1/distri/internal/squashfs"
	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"
	"github.com/google/renameio"
	"golang.org/x/exp/mmap"
	"golang.org/x/sync/errgroup"
	"golang.org/x/xerrors"
	"google.golang.org/grpc"
)

// totalBytes counts the number of bytes written to the disk for this install
// operation.
var totalBytes int64

type errPackageNotFound struct {
	pkg string
}

func (e errPackageNotFound) Error() string {
	return fmt.Sprintf("package %s not found on any configured repo", e.pkg)
}

func isNotExist(err error) bool {
	if _, ok := err.(*repo.ErrNotFound); ok {
		return true
	}
	return os.IsNotExist(err)
}

func unpackDir(dest string, rd *squashfs.Reader, inode squashfs.Inode) error {
	if err := os.MkdirAll(dest, 0755); err != nil {
		return err
	}
	fis, err := rd.Readdir(inode)
	if err != nil {
		return err
	}
	for _, fi := range fis {
		destName := filepath.Join(dest, fi.Name())
		fileInode := fi.Sys().(*squashfs.FileInfo).Inode
		if fi.IsDir() {
			if err := unpackDir(destName, rd, fileInode); err != nil {
				return err
			}
		} else if fi.Mode()&os.ModeSymlink > 0 {
			target, err := rd.ReadLink(fileInode)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, destName); err != nil {
				if os.IsExist(err) {
					got, err := os.Readlink(destName)
					if err != nil {
						return err
					}
					if target != got {
						if err := os.Remove(destName); err != nil {
							log.Printf("remove(%s): %v", destName, err)
						}
						return os.Symlink(target, destName)
					}
					// fallthrough: target identical
				} else {
					return err
				}
			}
		} else if fi.Mode().IsRegular() {
			fr, err := rd.FileReader(fileInode)
			if err != nil {
				return err
			}
			f, err := os.OpenFile(destName, os.O_RDWR|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
			if err != nil {
				return err
			}
			n, err := io.Copy(f, fr)
			if err != nil {
				return err
			}
			atomic.AddInt64(&totalBytes, n)
			if err := f.Close(); err != nil {
				return err
			}
		} else {
			log.Printf("ERROR: unsupported SquashFS file type: %+v", fi.Mode())
		}
	}
	return nil
}

// Ctx is an install context, containing configuration and state.
type Ctx struct {
	// Configuration
	SkipContentHooks bool
	HookDryRun       io.Writer // if non-nil, write commands instead of executing
}

func (c *Ctx) install1(ctx context.Context, root string, installRepo distri.Repo, pkg string, first bool) error {
	if _, err := os.Stat(filepath.Join(root, "roimg", pkg+".squashfs")); err == nil {
		return nil // package already installed
	}

	tmpDir := filepath.Join(root, "roimg", "tmp", "."+pkg+fmt.Sprintf("%d", os.Getpid()))
	if err := os.Mkdir(tmpDir, 0755); err != nil {
		if os.IsExist(err) {
			return nil // another goroutine is installing this package
		}
		return err
	}

	log.Printf("installing package %q to root %s", pkg, root)

	for _, fn := range []string{pkg + ".squashfs", pkg + ".meta.textproto"} {
		f, err := os.Create(filepath.Join(tmpDir, fn))
		if err != nil {
			return err
		}
		in, err := repo.Reader(ctx, installRepo, "pkg/"+fn, false)
		if err != nil {
			return err
		}
		defer in.Close()
		n, err := io.Copy(f, in)
		if err != nil {
			return err
		}
		atomic.AddInt64(&totalBytes, n)
		in.Close()
		if err := f.Close(); err != nil {
			return err
		}
	}

	// first is true only on the first installation of the package (regardless
	// of its version).
	if first {
		readerAt, err := mmap.Open(filepath.Join(tmpDir, pkg+".squashfs"))
		if err != nil {
			return xerrors.Errorf("copying /etc: %v", err)
		}
		defer readerAt.Close()

		rd, err := squashfs.NewReader(readerAt)
		if err != nil {
			return err
		}

		fis, err := rd.Readdir(rd.RootInode())
		if err != nil {
			return err
		}
		for _, fi := range fis {
			if fi.Name() != "etc" {
				continue
			}
			log.Printf("copying %s/etc", pkg)
			if err := unpackDir(filepath.Join(root, "etc"), rd, fi.Sys().(*squashfs.FileInfo).Inode); err != nil {
				return xerrors.Errorf("copying /etc: %v", err)
			}
			break
		}
	}

	hookinstall := func(dest, src string) error {
		readerAt, err := mmap.Open(filepath.Join(tmpDir, pkg+".squashfs"))
		if err != nil {
			return xerrors.Errorf("copying %s: %v", src, err)
		}
		defer readerAt.Close()

		rd, err := squashfs.NewReader(readerAt)
		if err != nil {
			return err
		}

		inode, err := rd.LookupPath(src)
		if err != nil {
			return err
		}

		r, err := rd.FileReader(inode)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
		f, err := renameio.TempFile("", dest)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, r); err != nil {
			return err
		}
		if strings.HasSuffix(dest, "/init") {
			f.Chmod(0755)
		}
		if err := f.CloseAtomicallyReplace(); err != nil {
			return err
		}
		return nil
	}

	// hook: distri1
	if strings.HasPrefix(pkg, "distri1-") && distri.ParseVersion(pkg).Pkg == "distri1" {
		log.Println("hook/distri1: updating /init")
		if err := hookinstall(filepath.Join(root, "init"), "out/bin/distri"); err != nil {
			return err
		}
	}

	// hook: linux
	if strings.HasPrefix(pkg, "linux-") {
		pv := distri.ParseVersion(pkg)
		if pv.Pkg == "linux" {
			version := fmt.Sprintf("%s-%d", pv.Upstream, pv.DistriRevision)
			dest := filepath.Join(root, "boot", "vmlinuz-"+version)
			log.Printf("hook/linux: updating %s", dest)
			if err := hookinstall(dest, "out/vmlinuz"); err != nil {
				return err
			}

			if root == "/" || c.HookDryRun != nil {
				distri.RegisterAtExit(func() error {
					initramfsGenerator := "minitrd"
					b, err := ioutil.ReadFile(filepath.Join(root, "etc", "distri", "initramfs-generator"))
					if err == nil {
						initramfsGenerator = strings.TrimSpace(string(b))
					}
					initramfs := "/boot/initramfs-" + pv.Upstream + "-" + strconv.FormatInt(pv.DistriRevision, 10) + ".img"
					var cmd *exec.Cmd
					switch initramfsGenerator {
					case "dracut":
						cmd = exec.Command("sh", "-c", "dracut --force "+initramfs+" "+pv.Upstream)

					case "minitrd":
						cmd = exec.Command("sh", "-c", "distri initrd -release "+pv.Upstream+" -output "+initramfs)

					default:
						return fmt.Errorf("unknown initramfs generator %v", initramfsGenerator)
					}
					cmd.Stderr = os.Stderr
					cmd.Stdout = os.Stdout
					log.Printf("hook/linux: running %v", cmd.Args)
					if c.HookDryRun != nil {
						fmt.Fprintf(c.HookDryRun, "%v\n", cmd.Args)
					} else {
						if err := cmd.Run(); err != nil {
							return fmt.Errorf("%v: %v", cmd.Args, err)
						}
					}
					return nil
				})
				distri.RegisterAtExit(func() error {
					cmd := exec.Command("/etc/update-grub")
					log.Printf("hook/linux: running %v", cmd.Args)
					cmd.Stdout = os.Stdout
					cmd.Stderr = os.Stderr
					if c.HookDryRun != nil {
						fmt.Fprintf(c.HookDryRun, "%v\n", cmd.Args)
					} else {
						if err := cmd.Run(); err != nil {
							return xerrors.Errorf("%v: %w", cmd.Args, err)
						}
					}
					return nil
				})
			}
		}
	}
	if strings.HasPrefix(pkg, "intel-ucode-") ||
		strings.HasPrefix(pkg, "amd-ucode-") {
		pv := distri.ParseVersion(pkg)
		if pv.Pkg == "intel-ucode" ||
			pv.Pkg == "amd-ucode" {
			base := "intel-ucode.img"
			if pv.Pkg == "amd-ucode" {
				base = "amd-ucode.img"
			}
			dest := filepath.Join(root, "boot", base)

			log.Printf("hook/ucode: updating %s", dest)
			if err := hookinstall(dest, "out/boot/"+base); err != nil {
				return err
			}
		}
	}

	readerAt, err := mmap.Open(filepath.Join(tmpDir, pkg+".squashfs"))
	if err != nil {
		return err
	}
	defer readerAt.Close()

	rd, err := squashfs.NewReader(readerAt)
	if err != nil {
		return err
	}

	if !c.SkipContentHooks {
		if _, err := rd.LookupPath("out/lib/sysusers.d"); err == nil {
			distri.RegisterAtExit(func() error {
				path, err := exec.LookPath("systemd-sysusers")
				if err != nil {
					log.Printf("systemd-sysusers not found, not creating users")
					return nil
				}
				cmd := exec.Command(path, "--root="+root)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err != nil {
					return xerrors.Errorf("%v: %v", cmd.Args, err)
				}
				return nil
			})
		}

		if _, err := rd.LookupPath("out/lib/tmpfiles.d"); err == nil {
			distri.RegisterAtExit(func() error {
				path, err := exec.LookPath("systemd-tmpfiles")
				if err != nil {
					log.Printf("systemd-tmpfiles not found, not creating tmpfiles")
					return nil
				}
				cmd := exec.Command(path, "--create", "--root="+root)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err != nil {
					return xerrors.Errorf("%v: %v", cmd.Args, err)
				}
				return nil
			})
		}
	}

	// First meta, then image: the fuse daemon considers the image canonical, so
	// it must go last.
	for _, fn := range []string{pkg + ".meta.textproto", pkg + ".squashfs"} {
		if err := os.Rename(filepath.Join(tmpDir, fn), filepath.Join(root, "roimg", fn)); err != nil {
			return err
		}
	}

	if err := os.Remove(tmpDir); err != nil {
		return err
	}

	return nil
}

func (c *Ctx) installTransitively1(root string, repos []distri.Repo, pkg string) error {
	origpkg := pkg
	if _, ok := distri.HasArchSuffix(pkg); !ok && !distri.LikelyFullySpecified(pkg) {
		pkg += "-amd64" // TODO: configurable / auto-detect
	}
	metas := make(map[*pb.Meta]distri.Repo)
	for _, r := range repos {
		rd, err := repo.Reader(context.Background(), r, "pkg/"+pkg+".meta.textproto", false)
		if err != nil {
			if isNotExist(err) {
				continue
			}
			return err
		}
		b, err := ioutil.ReadAll(rd)
		rd.Close()
		if err != nil {
			return err
		}
		var pm pb.Meta
		if err := proto.UnmarshalText(string(b), &pm); err != nil {
			return err
		}
		metas[&pm] = r
	}
	var pm *pb.Meta
	var repo distri.Repo
	for m, r := range metas {
		if pm == nil || m.GetVersion() > pm.GetVersion() {
			pm = m
			repo = r
		}
	}
	if pm == nil {
		return &errPackageNotFound{pkg: pkg}
	}

	if _, ok := distri.HasArchSuffix(pkg); ok {
		pkg += "-" + pm.GetVersion()
	}

	// TODO(later): we could write out b here and save 1 HTTP request
	pkgs := append([]string{pkg}, pm.GetRuntimeDep()...)
	log.Printf("resolved %s to %v", origpkg, pkgs)

	// TODO: figure out if this is the first installation by checking existence
	// in the corresponding pkgset file
	first := true

	// download all packages with maximum concurrency for the time being
	var eg errgroup.Group
	for _, pkg := range pkgs {
		pkg := pkg //copy
		eg.Go(func() error {
			var err error
			labels := pprof.Labels("package", pkg)
			pprof.Do(context.Background(), labels, func(ctx context.Context) {
				err = c.install1(ctx, root, repo, pkg, first)
			})
			if err != nil {
				return fmt.Errorf("installing %s: %v", pkg, err)
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}

	// TODO(later): tell the FUSE daemon that a (single) new package is
	// available so that new packages can be used while a bunch of them are
	// being installed?

	return nil
}

func (c *Ctx) Packages(args []string, root, repo string, update bool) error {
	atomic.StoreInt64(&totalBytes, 0)

	repos, err := env.Repos()
	if err != nil {
		return err
	}
	if repo != "" {
		repos = []distri.Repo{{Path: repo}}
	}
	if len(repos) == 0 {
		return xerrors.Errorf("no repos configured")
	}

	// TODO: lock to ensure only one process modifies roimg at a time

	tmpDir := filepath.Join(root, "roimg", "tmp")

	// Remove stale work directories of previously interrupted/crashed processes.
	if err := os.RemoveAll(tmpDir); err != nil {
		return err
	}

	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return err
	}

	start := time.Now()
	defer func() {
		dur := time.Since(start)

		total := atomic.LoadInt64(&totalBytes)
		log.Printf("done, %.2f MB/s (%v bytes in %v)", float64(total)/1024/1024/(float64(dur)/float64(time.Second)), total, dur)
	}()

	var eg errgroup.Group
	for _, pkg := range args {
		pkg := pkg // copy
		eg.Go(func() error {
			err := c.installTransitively1(root, repos, pkg)
			if _, ok := err.(*errPackageNotFound); ok && update {
				return nil // ignore package not found
			}
			return err
		})
	}
	ctx := context.Background()
	var cl pb.FUSEClient
	eg.Go(func() error {
		// Make the FUSE daemon update its packages.
		ctl, err := os.Readlink(filepath.Join(root, "ro", "ctl"))
		if err != nil {
			log.Printf("not updating FUSE daemon: %v", err)
			return nil // no FUSE daemon running?
		}

		log.Printf("connecting to %s", ctl)

		conn, err := grpc.DialContext(ctx, "unix://"+ctl, grpc.WithBlock(), grpc.WithInsecure())
		if err != nil {
			return err
		}
		cl = pb.NewFUSEClient(conn)
		return nil
	})
	if err := eg.Wait(); err != nil {
		return err
	}

	if cl != nil {
		if _, err := cl.ScanPackages(ctx, &pb.ScanPackagesRequest{}); err != nil {
			return err
		}
	}

	return nil
}
