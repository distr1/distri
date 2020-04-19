package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/build"
	"github.com/distr1/distri/internal/env"
	cmdfuse "github.com/distr1/distri/internal/fuse"
	"github.com/distr1/distri/internal/install"
	"github.com/jacobsa/fuse"
	"golang.org/x/sys/unix"
	"golang.org/x/xerrors"
)

const packHelp = `distri pack [-flags]

Pack a distri system image (for a USB memory stick, qemu, cloud, …).

This command is typically invoked through the distri Makefile:

Example:
  % make image serial=1
  % make qemu-serial
`

func passwd(root string) string {
	loginShell := "/bin/sh"
	if m, err := filepath.Glob(filepath.Join(root, "roimg", "zsh-*.meta.textproto")); err == nil && len(m) > 0 {
		loginShell = "/bin/zsh"
	}
	return "root:x:0:0:root:/root:" + loginShell + "\n"
}

const group = `root:x:0:
`

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

type packctx struct {
	root                 string
	repo                 string
	extraBase            string
	diskImg              string
	diskImgSize          int64
	gcsDiskImg           string
	encrypt              bool
	lvm                  bool
	serialOnly           bool
	bootDebug            bool
	branch               string
	overrideRepo         string
	rootPassword         string
	cryptPassword        string
	docker               bool
	authorizedKeys       string
	initramfsGenerator   string
	extraKernelParams    string
	extraLVMHook         string
	overwriteBlockDevice string
}

func pack(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("pack", flag.ExitOnError)
	var p packctx
	fset.StringVar(&p.root, "root",
		"",
		"TODO")
	fset.StringVar(&p.repo, "repo", env.DefaultRepo, "TODO")
	fset.StringVar(&p.extraBase, "base", "", "if non-empty, an additional base image to install")
	fset.StringVar(&p.diskImg, "diskimg", "", "Write an ext4 file system image to the specified path")
	fset.Int64Var(&p.diskImgSize, "diskimg_size", 7*1024*1024*1024 /* 7 GiB */, "Disk image size in bytes (default: 7 GiB)")
	fset.StringVar(&p.gcsDiskImg, "gcsdiskimg", "", "Write a Google Cloud file system image (tar.gz containing disk.raw) to the specified path")
	fset.BoolVar(&p.encrypt, "encrypt", false, "Whether to encrypt the image’s partitions (with LUKS)")
	fset.BoolVar(&p.lvm, "lvm", false, "Whether to place the root file system on an LVM logical volume")
	fset.BoolVar(&p.serialOnly, "serialonly", false, "Whether to print output only on console=ttyS0,115200 (defaults to false, i.e. console=tty1)")
	fset.BoolVar(&p.bootDebug, "bootdebug", false, "Whether to debug early boot, i.e. add systemd.log_level=debug systemd.log_target=console")
	fset.StringVar(&p.branch, "branch", "master", "Which git branch to track in repo URL")
	fset.StringVar(&p.overrideRepo, "override_repo", "", "If non-empty, overrides the default repo URL (see -branch)")
	fset.StringVar(&p.rootPassword, "root_password", "peace", "password to set for the root account")
	fset.StringVar(&p.cryptPassword, "crypt_password", "peace", "disk encryption password to use with -encrypt")
	fset.BoolVar(&p.docker, "docker", false, "generate a tar ball to feed to docker import")
	fset.StringVar(&p.authorizedKeys, "authorized_keys", "", "if non-empty, path to an SSH authorized_keys file to include for the root user")
	fset.StringVar(&p.initramfsGenerator, "initramfs_generator", "minitrd", "Which initramfs generator to use: minitrd or dracut. Chose minitrd for fastest initramfs generation and boot, chose dracut for customizeability or features that minitrd does not implement.")
	fset.StringVar(&p.extraKernelParams, "extra_kernel_params", "", "extra Linux kernel parameters to append to the kernel command line")
	fset.StringVar(&p.extraLVMHook, "extra_lvm_hook", "", "path to an executable program that modifies the LVM setup after the distri installer created it")
	fset.StringVar(&p.overwriteBlockDevice, "overwrite_block_device", "", "path to a block device to overwrite")
	fset.Usage = usage(fset, packHelp)
	fset.Parse(args)

	if p.gcsDiskImg == "" &&
		p.diskImg == "" &&
		!p.docker &&
		p.overwriteBlockDevice == "" {
		if p.root == "" {
			return xerrors.Errorf("syntax: pack -root=<directory>")
		}

		if err := p.pack(p.root); err != nil {
			return err
		}
	}

	if p.overwriteBlockDevice != "" {
		loop, err := openLoop(p.overwriteBlockDevice, 0)
		if err != nil {
			return err
		}
		if err := p.overwriteBlockDevice0(loop, func() (*os.File, error) {
			return openLoop(p.overwriteBlockDevice, 0)
		}); err != nil {
			return fmt.Errorf("overwriteBlockDevice0: %v", err)
		}
		return nil
	}

	if p.gcsDiskImg != "" && p.diskImg == "" {
		// Creating a Google Cloud disk image requires creating a disk image
		// first, so use a temporary file:
		tmp, err := ioutil.TempFile("", "distriimg")
		if err != nil {
			return err
		}
		tmp.Close()
		defer os.Remove(tmp.Name())
		p.diskImg = tmp.Name()
	}

	if p.diskImg != "" {
		if err := p.writeDiskImg(p.diskImgSize); err != nil {
			return xerrors.Errorf("writeDiskImg: %v", err)
		}
	}

	if p.gcsDiskImg != "" {
		log.Printf("Writing Google Cloud disk image to %s", p.gcsDiskImg)
		img, err := os.Open(p.diskImg)
		if err != nil {
			return err
		}
		defer img.Close()
		st, err := img.Stat()
		if err != nil {
			return err
		}

		f, err := os.Create(p.gcsDiskImg)
		if err != nil {
			return err
		}
		defer f.Close()
		gw, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
		if err != nil {
			return err
		}
		tw := tar.NewWriter(gw)
		if err := tw.WriteHeader(&tar.Header{
			Name:   "disk.raw",
			Size:   st.Size(),
			Mode:   0644,
			Format: tar.FormatGNU,
		}); err != nil {
			return err
		}
		if _, err := io.Copy(tw, img); err != nil {
			return err
		}
		if err := tw.Close(); err != nil {
			return err
		}
		if err := gw.Close(); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}

	if p.docker {
		root, err := ioutil.TempDir("", "distridocker")
		if err != nil {
			return err
		}
		defer os.RemoveAll(root)

		c := &install.Ctx{
			SkipContentHooks: true,
		}
		if err := c.Packages([]string{
			"base",
			"rxvt-unicode",    // for its terminfo file
			"ca-certificates", // so that we can install packages via https
		}, root, p.repo, false); err != nil {
			return err
		}

		for _, dir := range []string{
			"etc",
			"etc/distri/repos.d",
			"ro",
			"ro-tmp",
		} {
			if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
				return err
			}
		}

		if err := ioutil.WriteFile(filepath.Join(root, "etc/passwd"), []byte(passwd(root)), 0644); err != nil {
			return err
		}

		distri1Repo := "https://repo.distr1.org/distri/" + p.branch
		if p.overrideRepo != "" {
			distri1Repo = p.overrideRepo
		}
		if err := ioutil.WriteFile(filepath.Join(root, "etc/distri/repos.d/distr1.repo"), []byte(distri1Repo+"\n"), 0644); err != nil {
			return err
		}

		type symlink struct {
			oldname, newname string
		}
		for _, link := range []symlink{
			{"/", "usr"},
			{"/ro/bin", "bin"},
			{"/ro/share", "share"},
			{"/ro/lib", "lib"},
			{"/ro/include", "include"},
			{"/ro/sbin", "sbin"},
			{"/init", "entrypoint"},
		} {
			if err := os.Symlink(link.oldname, filepath.Join(root, link.newname)); err != nil {
				return err
			}
		}

		// Remove packages we don’t need to reduce docker container size:
		b := &build.Ctx{
			Arch: "amd64",
			Repo: env.DefaultRepo,
		} // TODO: introduce a packctx, make glob take a common ctx
		resolved, err := b.Glob(p.repo, []string{
			"linux-firmware",
			"docker-engine",
			"dracut",
			"binutils",
			"elfutils",
		})
		if err != nil {
			return err
		}

		for _, pkg := range resolved {
			for _, ext := range []string{"squashfs", "meta.textproto"} {
				if err := os.Remove(filepath.Join(root, "roimg", pkg+"."+ext)); err != nil {
					return err
				}
			}
		}

		tar := exec.Command("tar", "-c", ".")
		tar.Dir = root
		tar.Stdout = os.Stdout
		tar.Stderr = os.Stderr
		if err := tar.Run(); err != nil {
			return fmt.Errorf("%v: %v", tar.Args, err)
		}
	}

	return nil
}

func (p *packctx) pack(root string) error {
	for _, dir := range []string{
		"etc",
		"root",
		"boot",    // grub
		"esp",     // grub (EFI System Partition)
		"dev",     // udev
		"ro",      // read-only package directory (mountpoint)
		"ro-dbg",  // read-only package directory (mountpoint)
		"src",     // read-only package directory (mountpoint)
		"roimg",   // read-only package store
		"rodebug", // read-only package store
		"rosrc",   // read-only package store
		"ro-tmp",  // temporary directory which is not hidden by systemd’s tmp.mount
		"proc",    // procfs
		"sys",     // sysfs
		"tmp",     // tmpfs
		"var/tmp", // systemd (e.g. systemd-networkd)
	} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			return err
		}
	}

	if err := os.Symlink("/run", filepath.Join(root, "var", "run")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := os.Symlink("/ro/bin", filepath.Join(root, "bin")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := os.Symlink("/ro/bin", filepath.Join(root, "sbin")); err != nil && !os.IsExist(err) {
		return err
	}

	// We run systemd in non-split mode, so /usr needs to point to /
	if err := os.Symlink("/", filepath.Join(root, "usr")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := os.Symlink("/ro/lib", filepath.Join(root, "lib")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := os.Symlink("/ro/share", filepath.Join(root, "share")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := os.Symlink("/ro/include", filepath.Join(root, "include")); err != nil && !os.IsExist(err) {
		return err
	}

	// TODO: de-duplicate with build.go
	if err := os.Symlink("/ro/glibc-amd64-2.31-4/out/lib", filepath.Join(root, "lib64")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := ioutil.WriteFile(filepath.Join(root, "etc/resolv.conf"), []byte("nameserver 8.8.8.8\nnameserver 2001:4860:4860::8888\n"), 0644); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Join(root, "etc/distri/repos.d"), 0755); err != nil {
		return err
	}
	distri1Repo := "https://repo.distr1.org/distri/" + p.branch
	if p.overrideRepo != "" {
		distri1Repo = p.overrideRepo
	}

	if err := ioutil.WriteFile(filepath.Join(root, "etc/distri/repos.d/distr1.repo"), []byte(distri1Repo+"\n"), 0644); err != nil {
		return err
	}

	if err := ioutil.WriteFile(filepath.Join(root, "etc/os-release"), []byte(`ID=distri
VERSION_CODENAME=`+p.branch+`
PRETTY_NAME="distri (`+p.branch+`)"
HOME_URL=https://distr1.org
`), 0644); err != nil {
		return err
	}

	if p.authorizedKeys != "" {
		if err := os.MkdirAll(filepath.Join(root, "root/.ssh"), 0700); err != nil {
			return err
		}
		if err := copyFile(p.authorizedKeys, filepath.Join(root, "root/.ssh/authorized_keys")); err != nil {
			return err
		}
	}

	b := &build.Ctx{
		Arch: "amd64",
		Repo: env.DefaultRepo,
	} // TODO: introduce a packctx, make glob take a common ctx

	basePkgNames := []string{"base"} // contains packages required for pack
	if p.extraBase != "" {
		basePkgNames = append(basePkgNames, p.extraBase)
		pkgset := filepath.Join(root, "etc", "distri", "pkgset.d", "extrabase.pkgset")
		if err := os.MkdirAll(filepath.Dir(pkgset), 0755); err != nil {
			return err
		}
		if err := ioutil.WriteFile(pkgset, []byte(p.extraBase+"\n"), 0644); err != nil {
			return err
		}
	}

	basePkgs, err := b.Glob(p.repo, basePkgNames)
	if err != nil {
		return err
	}

	c := &install.Ctx{
		SkipContentHooks: true,
	}
	if err := c.Packages(basePkgs, root, p.repo, false); err != nil {
		return err
	}

	if err := ioutil.WriteFile(filepath.Join(root, "etc/passwd"), []byte(passwd(root)), 0644); err != nil {
		return err
	}
	if err := ioutil.WriteFile(filepath.Join(root, "etc/group"), []byte(group), 0644); err != nil {
		return err
	}

	ctx, canc := context.WithCancel(context.Background())
	defer canc()
	if _, err := cmdfuse.Mount(ctx, []string{"-repo=" + filepath.Join(root, "roimg"), filepath.Join(root, "ro")}); err != nil {
		return err
	}
	defer fuse.Unmount(filepath.Join(root, "ro"))

	// XXX: this is required for systemd-firstboot
	cmdline := filepath.Join(root, "proc", "cmdline")
	if err := ioutil.WriteFile(cmdline, []byte("systemd.firstboot=1"), 0644); err != nil {
		return err
	}
	defer os.Remove(cmdline)
	cmd := exec.Command("unshare",
		"--user",
		"--map-root-user", // for mount permissions in the namespace
		"--mount",
		"--",
		"chroot", root, "/ro/bin/systemd-firstboot", "--hostname=distri0",
		"--root-password="+p.rootPassword,
		"--copy-timezone",
		"--copy-locale",
		"--setup-machine-id")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return xerrors.Errorf("%v: %w", cmd.Args, err)
	}

	cmd = exec.Command("unshare",
		"--user",
		"--map-root-user", // for mount permissions in the namespace
		"--mount",
		"--",
		"chroot", root, "/ro/bin/systemd-sysusers",
		"/ro/lib/sysusers.d/basic.conf",
		"/ro/lib/sysusers.d/systemd.conf")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return xerrors.Errorf("%v: %w", cmd.Args, err)
	}

	// TODO: dynamically find which units to enable (test: xdm)
	units := []string{
		"systemd-networkd",
		"containerd",
		"docker",
		"ssh",
		"haveged",
	}
	if p.extraBase == "base-x11" {
		units = append(units, "debugfs", "srcfs")
	}
	cmd = exec.Command("unshare",
		append([]string{
			"--user",
			"--map-root-user", // for mount permissions in the namespace
			"--mount",
			"--",
			"chroot", root, "/ro/bin/systemctl",
			"enable",
		}, units...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return xerrors.Errorf("%v: %w", cmd.Args, err)
	}

	pamd := filepath.Join(root, "etc", "pam.d")
	if err := os.MkdirAll(pamd, 0755); err != nil {
		return err
	}

	const pamdOther = `auth	required	pam_unix.so
auth	required	pam_warn.so
account	required	pam_unix.so
account	required	pam_warn.so

# success=1 will skip the pam_warn.so line
password	[success=1 default=ignore]	pam_unix.so
password	requisite	pam_warn.so
password	required	pam_permit.so

session	required	pam_unix.so
session	optional	pam_systemd.so
session	required	pam_warn.so
`
	if err := ioutil.WriteFile(filepath.Join(pamd, "other"), []byte(pamdOther), 0644); err != nil {
		return err
	}
	if err := os.Symlink("other", filepath.Join(pamd, "system-auth")); err != nil && !os.IsExist(err) {
		return err
	}

	const dbusSystemLocal = `<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <includedir>/ro/share/dbus-1/system.d</includedir>
</busconfig>
`
	if err := ioutil.WriteFile(filepath.Join(root, "etc", "dbus-1", "system-local.conf"), []byte(dbusSystemLocal), 0644); err != nil {
		return err
	}

	const nsswitch = `passwd:         compat mymachines systemd
group:          compat mymachines systemd
shadow:         compat

hosts:          files mymachines resolve [!UNAVAIL=return] dns  myhostname
networks:       files

protocols:      db files
services:       db files
ethers:         db files
rpc:            db files

netgroup:       nis
`
	if err := ioutil.WriteFile(filepath.Join(root, "etc", "nsswitch.conf"), []byte(nsswitch), 0644); err != nil {
		return err
	}

	if err := adduser(root, "systemd-network:x:101:101:network:/run/systemd/netif:/bin/false"); err != nil {
		return err
	}
	if err := addgroup(root, "systemd-network:x:103:"); err != nil {
		return err
	}
	if err := adduser(root, "systemd-resolve:x:105:105:resolve:/run/systemd/resolve:/bin/false"); err != nil {
		return err
	}
	if err := addgroup(root, "systemd-resolve:x:105:"); err != nil {
		return err
	}

	if err := adduser(root, "sshd:x:102:102:sshd:/:/bin/false"); err != nil {
		return err
	}

	if err := adduser(root, "messagebus:x:106:106:messagebus:/var/run/dbus:/bin/false"); err != nil {
		return err
	}

	if err := addgroup(root, "docker:x:104:"); err != nil {
		return err
	}

	if err := addgroup(root, "messagebus:x:106:"); err != nil {
		return err
	}

	// TODO: once https://github.com/systemd/systemd/issues/3998 is fixed, use
	// their catch-all file rather than ours.
	network := filepath.Join(root, "etc", "systemd", "network")
	if err := os.MkdirAll(network, 0755); err != nil {
		return err
	}

	if err := ioutil.WriteFile(filepath.Join(network, "en.network"), []byte(`
[Match]
#Type=ether
Name=en*

[Network]
DHCP=yes
`), 0644); err != nil {
		return err
	}

	if err := ioutil.WriteFile(filepath.Join(network, "eth.network"), []byte(`
[Match]
#Type=ether
Name=eth*

[Network]
DHCP=yes
`), 0644); err != nil {
		return err
	}

	modules := filepath.Join(root, "etc", "modules-load.d")
	if err := os.MkdirAll(modules, 0755); err != nil {
		return err
	}

	if err := ioutil.WriteFile(filepath.Join(modules, "docker.conf"), []byte(`
iptable_nat
ipt_MASQUERADE
xt_addrtype
veth
`), 0644); err != nil {
		return err
	}

	fuse.Unmount(filepath.Join(root, "ro"))

	chown := exec.Command("sh", "-c", fmt.Sprintf(`find "%s" -xdev -print0 | sudo xargs -0 chown --no-dereference --from="%s" root:root`, root, os.Getenv("USER")))
	chown.Stderr = os.Stderr
	chown.Stdout = os.Stdout
	if err := chown.Run(); err != nil {
		return xerrors.Errorf("%v: %v", chown.Args, err)
	}

	return nil
}

func (p *packctx) writeDiskImg(sz int64) error {
	f, err := os.OpenFile(p.diskImg, os.O_CREATE|os.O_TRUNC|os.O_RDWR|unix.O_CLOEXEC, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(sz); err != nil {
		return err
	}

	// Find the next free loop device:
	const (
		LOOP_CTL_GET_FREE = 0x4c82
	)

	loopctl, err := os.Open("/dev/loop-control")
	if err != nil {
		return err
	}
	defer loopctl.Close()
	free, _, errno := unix.Syscall(unix.SYS_IOCTL, loopctl.Fd(), LOOP_CTL_GET_FREE, 0)
	if errno != 0 {
		return errno
	}
	loopctl.Close()
	log.Printf("next free: %d", free)

	loopdev := fmt.Sprintf("/dev/loop%d", free)
	loop, err := openLoop(loopdev, uintptr(f.Fd()))
	if err != nil {
		return err
	}
	var cleanup string
	defer func() {
		if cleanup == "" {
			return
		}
		losetup := exec.Command("sudo", "losetup", "-d", cleanup)
		losetup.Stdout = os.Stdout
		losetup.Stderr = os.Stderr
		log.Println(losetup.Args)
		if err := losetup.Run(); err != nil {
			log.Printf("%v: %v", losetup.Args, err)
		}
	}()

	return p.overwriteBlockDevice0(loop, func() (*os.File, error) {
		loop.Close()
		f.Close()
		// TODO: implement partscan in native Go
		losetup := exec.Command("sudo", "losetup", "--show", "--find", "--partscan", p.diskImg)
		losetup.Stderr = os.Stderr
		out, err := losetup.Output()
		if err != nil {
			return nil, xerrors.Errorf("%v: %v", losetup.Args, err)
		}
		base := strings.TrimSpace(string(out))
		cleanup = base
		log.Printf("base: %q", base)
		return os.OpenFile(base, os.O_RDWR|unix.O_CLOEXEC, 0644)
	})
}

func openLoop(loopdev string, fd uintptr) (*os.File, error) {
	loop, err := os.OpenFile(loopdev, os.O_RDWR|unix.O_CLOEXEC, 0644)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(loopdev, "/dev/loop") {
		return loop, nil
	}
	// TODO: get this into x/sys/unix
	type LoopInfo64 struct {
		device         uint64
		inode          uint64
		rdevice        uint64
		offset         uint64
		sizeLimit      uint64
		number         uint32
		encryptType    uint32
		encryptKeySize uint32
		flags          uint32
		filename       [64]byte
		cryptname      [64]byte
		encryptkey     [32]byte
		init           [2]uint64
	}
	const (
		LOOP_SET_FD       = 0x4c00
		LOOP_SET_STATUS64 = 0x4c04
	)
	const (
		LO_FLAGS_READ_ONLY = 1
		LO_FLAGS_AUTOCLEAR = 4 // loop device will autodestruct on last close
	)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, loop.Fd(), LOOP_SET_FD, fd); errno != 0 {
		return nil, fmt.Errorf("LOOP_SET_FD(%v): %v", fd, errno)
	}
	var filename [64]byte
	copy(filename[:], []byte("root"))
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, loop.Fd(), LOOP_SET_STATUS64, uintptr(unsafe.Pointer(&LoopInfo64{
		flags:    LO_FLAGS_AUTOCLEAR | LO_FLAGS_READ_ONLY,
		filename: filename,
	}))); errno != 0 {
		return nil, fmt.Errorf("LOOP_SET_STATUS64: %v", errno)
	}
	return loop, nil
}

func (p *packctx) overwriteBlockDevice0(loop *os.File, reopen func() (*os.File, error)) error {
	loopdev := loop.Name()
	defer func() {
		loop.Close()
	}()

	sfdisk := exec.Command("sudo", "sfdisk", loopdev)
	typeLinux := "0FC63DAF-8483-4772-8E79-3D69D8477DE4" // from sfdisk(8)
	if p.lvm {
		typeLinux = "E6D6D379-F507-44C2-A23C-238F2A3DF928" // from pvcreate(8)
	}
	sfdisk.Stdin = strings.NewReader(`label:gpt
size=550M,type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B
size=1M,type=21686148-6449-6E6F-744E-656564454649
size=250M, name=boot
name=root,type=` + typeLinux)
	sfdisk.Stdout = os.Stdout
	sfdisk.Stderr = os.Stderr
	if err := sfdisk.Run(); err != nil {
		return xerrors.Errorf("%v: %v", sfdisk.Args, err)
	}

	base := loopdev
	if p.diskImg != "" {
		var err error
		loop, err = reopen()
		if err != nil {
			return fmt.Errorf("re-opening: %v", err)
		}
		base = loop.Name()
	}

	esp := base + "p1"
	// p2 is the GRUB BIOS boot partition
	boot := base + "p3"
	root := base + "p4"

	mkfs := exec.Command("sudo", "mkfs.fat", "-F32", esp)
	mkfs.Stdout = os.Stdout
	mkfs.Stderr = os.Stderr
	if err := mkfs.Run(); err != nil {
		return xerrors.Errorf("%v: %v", mkfs.Args, err)
	}

	mkfs = exec.Command("sudo", "mkfs.ext2", boot)
	mkfs.Stdout = os.Stdout
	mkfs.Stderr = os.Stderr
	if err := mkfs.Run(); err != nil {
		return xerrors.Errorf("%v: %v", mkfs.Args, err)
	}

	if p.lvm {
		pvcreate := exec.Command("sudo", "pvcreate", "--verbose", "--yes", "-ff", root)
		pvcreate.Stdout = os.Stdout
		pvcreate.Stderr = os.Stderr
		log.Println(pvcreate.Args)
		if err := pvcreate.Run(); err != nil {
			return fmt.Errorf("%v: %v", pvcreate.Args, err)
		}

		vgcreate := exec.Command("sudo", "vgcreate", "--verbose", "distrivg", root)
		vgcreate.Stdout = os.Stdout
		vgcreate.Stderr = os.Stderr
		log.Println(vgcreate.Args)
		if err := vgcreate.Run(); err != nil {
			return fmt.Errorf("%v: %v", vgcreate.Args, err)
		}

		lvcreate := exec.Command("sudo", "lvcreate", "--verbose", "--extents=100%FREE", "--name=root", "distrivg")
		lvcreate.Stdout = os.Stdout
		lvcreate.Stderr = os.Stderr
		log.Println(lvcreate.Args)
		if err := lvcreate.Run(); err != nil {
			return fmt.Errorf("%v: %v", lvcreate.Args, err)
		}

		// TODO: can we activate the volume at a different path?
		// otherwise, this will fail on a distri system (see below)
		// - maybe use temp name and vgrename?
		// - look at how libguestfs works
		vgchange := exec.Command("sudo", "vgchange", "--verbose", "--activate=y", "distrivg")
		vgchange.Stdout = os.Stdout
		vgchange.Stderr = os.Stderr
		log.Println(vgchange.Args)
		if err := vgchange.Run(); err != nil {
			return fmt.Errorf("%v: %v", vgchange.Args, err)
		}
		defer func() {
			vgchange := exec.Command("sudo", "vgchange", "--verbose", "--activate=n", "distrivg")
			vgchange.Stdout = os.Stdout
			vgchange.Stderr = os.Stderr
			if err := vgchange.Run(); err != nil {
				log.Printf("%v: %v", vgchange.Args, err)
			}
		}()

		if p.extraLVMHook != "" {
			hook := exec.Command("sudo", p.extraLVMHook)
			hook.Stdout = os.Stdout
			hook.Stderr = os.Stderr
			if err := hook.Run(); err != nil {
				return fmt.Errorf("%v: %v", hook.Args, err)
			}
		}

		root = "/dev/distrivg/root"
	}

	districrypt := "districrypt"
	var luksUUID string
	if p.encrypt {
		// Find an available (on the host) name for the crypt root device:
		for {
			if _, err := os.Stat("/dev/mapper/" + districrypt); err != nil {
				if os.IsNotExist(err) {
					break
				}
				return err
			}
			// stat succeeded, i.e. name in use
			districrypt += "1"
		}

		luksFormat := exec.Command("sudo", "cryptsetup", "luksFormat", root, "-")
		luksFormat.Stdin = strings.NewReader(p.cryptPassword)
		luksFormat.Stdout = os.Stdout
		luksFormat.Stderr = os.Stderr
		log.Println(luksFormat.Args)
		if err := luksFormat.Run(); err != nil {
			return xerrors.Errorf("%v: %v", luksFormat.Args, err)
		}

		var err error
		luksUUID, err = uuid(root, "fs")
		if err != nil {
			return xerrors.Errorf("lsblk: %v", err)
		}

		luksOpen := exec.Command("sudo", "cryptsetup", "open", "--type=luks", "--key-file=-", root, districrypt)
		luksOpen.Stdin = strings.NewReader(p.cryptPassword)
		luksOpen.Stdout = os.Stdout
		luksOpen.Stderr = os.Stderr
		if err := luksOpen.Run(); err != nil {
			return xerrors.Errorf("%v: %v", luksOpen.Args, err)
		}
		defer func() {
			luksClose := exec.Command("sudo", "cryptsetup", "close", districrypt)
			luksClose.Stdout = os.Stdout
			luksClose.Stderr = os.Stderr
			if err := luksClose.Run(); err != nil {
				log.Printf("%v: %v", luksClose.Args, err)
			}
		}()

		root = "/dev/mapper/" + districrypt
	}

	mkfs = exec.Command("sudo", "mkfs.ext4", root)
	mkfs.Stdout = os.Stdout
	mkfs.Stderr = os.Stderr
	if err := mkfs.Run(); err != nil {
		return xerrors.Errorf("%v: %v", mkfs.Args, err)
	}

	for _, entry := range []struct {
		dest, src, fs string
		extraflags    uintptr
	}{
		{"/mnt", root, "ext4", 0},
		{"/mnt/boot", boot, "ext2", 0},
		{"/mnt/boot/efi", esp, "vfat", 0},
		{"/mnt/dev", "/dev", "", syscall.MS_BIND},
		{"/mnt/sys", "/sys", "", syscall.MS_BIND | syscall.MS_PRIVATE},
	} {
		if err := os.MkdirAll(entry.dest, 0755); err != nil {
			return err
		}
		if err := syscall.Mount(entry.src, entry.dest, entry.fs, syscall.MS_MGC_VAL|entry.extraflags, ""); err != nil {
			return xerrors.Errorf("mount %s %s: %v", entry.src, entry.dest, err)
		}
		defer syscall.Unmount(entry.dest, 0)
	}

	if err := p.pack("/mnt"); err != nil {
		return err
	}

	chown := exec.Command("sudo", "chown", os.Getenv("USER"), "/mnt/ro")
	chown.Stderr = os.Stderr
	chown.Stdout = os.Stdout
	if err := chown.Run(); err != nil {
		return xerrors.Errorf("%v: %v", chown.Args, err)
	}
	ctx, canc := context.WithCancel(context.Background())
	defer canc()
	join, err := cmdfuse.Mount(ctx, []string{"-repo=/mnt/roimg", "/mnt/ro"})
	if err != nil {
		return err
	}
	defer fuse.Unmount("/mnt/ro")

	if err := os.MkdirAll("/mnt/boot/grub", 0755); err != nil {
		return err
	}

	if p.encrypt {
		crypttab := fmt.Sprintf(districrypt+" UUID=%s none luks,discard\n", luksUUID)
		if err := ioutil.WriteFile("/mnt/etc/crypttab", []byte(crypttab), 0644); err != nil {
			return err
		}
	}

	{
		fstab := "/dev/mapper/" + districrypt + " / ext4 defaults,x-systemd.device-timeout=0 1 1\n"
		bootUUID, err := uuid(boot, "part")
		if err != nil {
			return xerrors.Errorf(`uuid(boot=%v, "part"): %v`, boot, err)
		}
		fstab = fstab + "PARTUUID=" + bootUUID + " /boot ext2 defaults 1 2\n"
		espUUID, err := uuid(esp, "part")
		if err != nil {
			return xerrors.Errorf(`uuid(esp=%v, "part"): %v`, esp, err)
		}
		fstab = fstab + "PARTUUID=" + espUUID + " /boot/efi vfat defaults 0 1\n"
		if err := ioutil.WriteFile("/mnt/etc/fstab", []byte(fstab), 0644); err != nil {
			return err
		}
	}

	{
		shells := strings.Join([]string{
			"/bin/zsh",
			"/bin/bash",
			"/bin/sh",
		}, "\n") + "\n"
		if err := ioutil.WriteFile("/mnt/etc/shells", []byte(shells), 0644); err != nil {
			return err
		}
	}

	if err := ioutil.WriteFile("/mnt/etc/dracut.conf.d/kbddir.conf", []byte("kbddir=/ro/share\n"), 0644); err != nil {
		return err
	}

	kernels, err := filepath.Glob("/mnt/ro/linux-amd64-*")
	if err != nil {
		return err
	}
	for _, kernel := range kernels {
		pv := distri.ParseVersion(kernel)
		upstream := pv.Upstream
		full := pv.Upstream + "-" + strconv.FormatInt(pv.DistriRevision, 10)
		switch p.initramfsGenerator {
		case "dracut":
			dracut := exec.Command("sudo", "chroot", "/mnt", "sh", "-c", "dracut /boot/initramfs-"+full+".img "+upstream)
			dracut.Stderr = os.Stderr
			dracut.Stdout = os.Stdout
			if err := dracut.Run(); err != nil {
				return xerrors.Errorf("%v: %v", dracut.Args, err)
			}

		case "minitrd":
			minitrd := exec.Command("sudo", "chroot", "/mnt", "sh", "-c", "distri initrd -release "+upstream+" -output /boot/initramfs-"+full+".img")
			minitrd.Stderr = os.Stderr
			minitrd.Stdout = os.Stdout
			if err := minitrd.Run(); err != nil {
				return xerrors.Errorf("%v: %v", minitrd.Args, err)
			}

		default:
			return xerrors.Errorf("unknown initramfs generator %v", p.initramfsGenerator)
		}
	}

	if err := ioutil.WriteFile("/mnt/etc/distri/initramfs-generator", []byte(p.initramfsGenerator+"\n"), 0644); err != nil {
		return err
	}

	var params []string
	if !p.serialOnly {
		params = append(params, "console=tty1")
	}
	if p.encrypt {
		params = append(params, "rd.luks=1 rd.luks.uuid="+luksUUID+" rd.luks.name="+luksUUID+"="+districrypt)
	}
	if p.bootDebug {
		params = append(params, "systemd.log_level=debug systemd.log_target=console")
	}
	cmdline := "console=ttyS0,115200 " + strings.Join(params, " ") + " init=/init systemd.setenv=PATH=/bin rw " + p.extraKernelParams
	if err := ioutil.WriteFile(p.diskImg+".cmdline", []byte(cmdline+"\n"), 0644); err != nil {
		return err
	}
	mkconfigSerial := ""
	if p.serialOnly {
		mkconfigSerial = "GRUB_TERMINAL=serial"
	}
	mkconfigCmd := fmt.Sprintf(`GRUB_CMDLINE_LINUX=%q `+mkconfigSerial+` grub-mkconfig -o /boot/grub/grub.cfg`, cmdline)
	mkconfig := exec.Command("sudo", "chroot", "/mnt", "sh", "-c", mkconfigCmd)
	mkconfig.Stderr = os.Stderr
	mkconfig.Stdout = os.Stdout
	if err := mkconfig.Run(); err != nil {
		return xerrors.Errorf("%v: %v", mkconfig.Args, err)
	}

	if err := ioutil.WriteFile("/mnt/etc/update-grub", []byte("#!/bin/sh\n"+mkconfigCmd+"\n"), 0755); err != nil {
		return xerrors.Errorf("writing /etc/update-grub: %v", err)
	}

	install := exec.Command("sudo", "chroot", "/mnt", "sh", "-c", "/ro/grub2-amd64-*/bin/grub-install --target=i386-pc "+base)
	install.Stderr = os.Stderr
	install.Stdout = os.Stdout
	if err := install.Run(); err != nil {
		return xerrors.Errorf("%v: %v", install.Args, err)
	}

	install = exec.Command("sudo", "chroot", "/mnt", "sh", "-c", "/ro/grub2-efi-amd64-*/bin/grub-install --target=x86_64-efi --efi-directory=/boot/efi --removable --no-nvram --boot-directory=/boot")
	install.Stderr = os.Stderr
	install.Stdout = os.Stdout
	if err := install.Run(); err != nil {
		return xerrors.Errorf("%v: %v", install.Args, err)
	}

	if err := fuse.Unmount("/mnt/ro"); err != nil {
		return xerrors.Errorf("unmount /mnt/ro: %v", err)
	}

	if err := join(context.Background()); err != nil {
		return xerrors.Errorf("fuse: %v", err)
	}

	chown = exec.Command("sudo", "chown", "root", "/mnt/ro")
	chown.Stderr = os.Stderr
	chown.Stdout = os.Stdout
	if err := chown.Run(); err != nil {
		return xerrors.Errorf("%v: %v", chown.Args, err)
	}

	for _, m := range []string{"sys", "dev", "boot/efi", "boot", ""} {
		if err := syscall.Unmount(filepath.Join("/mnt", m), 0); err != nil {
			return xerrors.Errorf("unmount /mnt/%s: %v", m, err)
		}
	}

	return nil
}

func adduser(root, line string) error {
	// TODO: pam requires an entry in /etc/shadow, too, even if the password is disabled
	f, err := os.OpenFile(filepath.Join(root, "etc", "passwd"), os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write([]byte(line + "\n")); err != nil {
		return err
	}
	return f.Close()
}

func addgroup(root, line string) error {
	f, err := os.OpenFile(filepath.Join(root, "etc", "group"), os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write([]byte(line + "\n")); err != nil {
		return err
	}
	return f.Close()
}

func uuid(blockdev, kind string) (string, error) {
	st, err := os.Stat(blockdev)
	if err != nil {
		return "", err
	}
	rdev := st.Sys().(*syscall.Stat_t).Rdev
	const (
		// hard-coded, as in systemd-241/src/libsystemd/sd-device/sd-device.c
		udevDb = "/run/udev/data/b%d:%d"
	)
	b, err := ioutil.ReadFile(fmt.Sprintf(udevDb, unix.Major(rdev), unix.Minor(rdev)))
	if err != nil {
		return "", err
	}
	prefix := "E:ID_FS_UUID_ENC=" // kind == fs
	if kind == "part" {
		prefix = "E:ID_PART_ENTRY_UUID="
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		return strings.TrimPrefix(line, prefix), nil
	}
	return "", nil
}
