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
	"strings"
	"syscall"
	"unsafe"

	"github.com/jacobsa/fuse"
	"golang.org/x/sys/unix"
)

const packHelp = `TODO
`

const passwd = `root:x:0:0:root:/root:/bin/sh
`
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
		r, err := resolve1(imgDir, pkg, seen)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, r...)
	}
	return resolved, nil
}

func pack(args []string) error {
	fset := flag.NewFlagSet("pack", flag.ExitOnError)
	var (
		root = fset.String("root",
			"",
			"TODO")
		imgDir     = fset.String("imgdir", defaultImgDir, "TODO")
		diskImg    = fset.String("diskimg", "", "Write an ext4 file system image to the specified path")
		gcsDiskImg = fset.String("gcsdiskimg", "", "Write a Google Cloud file system image (tar.gz containing disk.raw) to the specified path")
		//pkg = fset.String("pkg", "", "path to .squashfs package to mount")
	)
	fset.Parse(args)
	if *root == "" {
		return fmt.Errorf("syntax: pack -root=<directory>")
	}

	for _, dir := range []string{
		"etc",
		"root",
		"boot",        // grub
		"dev",         // udev
		"ro",          // read-only package directory (mountpoint)
		"roimg",       // read-only package store
		"proc",        // procfs
		"sys",         // sysfs
		"tmp",         // tmpfs
		"var/tmp",     // systemd (e.g. systemd-networkd)
		"lib/systemd", // systemd
		"etc/ssl",     // openssl
	} {
		if err := os.MkdirAll(filepath.Join(*root, dir), 0755); err != nil {
			return err
		}
	}

	if err := os.Symlink("/run", filepath.Join(*root, "var", "run")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := os.Symlink("/ro/bin", filepath.Join(*root, "bin")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := os.Symlink("/ro/bin", filepath.Join(*root, "sbin")); err != nil && !os.IsExist(err) {
		return err
	}

	// We run systemd in non-split mode, so /usr needs to point to /
	if err := os.Symlink("/", filepath.Join(*root, "usr")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := os.Symlink("/ro/lib/systemd/system", filepath.Join(*root, "lib", "systemd", "system")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := os.Symlink("/ro/linux-4.18.7/buildoutput/lib/modules", filepath.Join(*root, "lib", "modules")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := os.Symlink("/ro/ca-certificates-3.39/buildoutput/etc/ssl/certs", filepath.Join(*root, "etc", "ssl", "certs")); err != nil && !os.IsExist(err) {
		return err
	}

	// TODO: de-duplicate with build.go
	if err := os.Symlink("/ro/glibc-2.27/buildoutput/lib", filepath.Join(*root, "lib64")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := copyFile("/proc/self/exe", filepath.Join(*root, "init")); err != nil {
		return err
	}

	if err := os.Chmod(filepath.Join(*root, "init"), 0755); err != nil {
		return err
	}

	if err := ioutil.WriteFile(filepath.Join(*root, "etc/resolv.conf"), []byte("nameserver 8.8.8.8"), 0644); err != nil {
		return err
	}

	if err := ioutil.WriteFile(filepath.Join(*root, "etc/passwd"), []byte(passwd), 0644); err != nil {
		return err
	}
	if err := ioutil.WriteFile(filepath.Join(*root, "etc/group"), []byte(group), 0644); err != nil {
		return err
	}

	basePkgs, err := resolve(*imgDir, []string{
		"systemd-239",
		"glibc-2.27",
		"coreutils-8.30",
		"strace-4.24",
		"bash-4.4.18",
		"psmisc-23.1",
		"ncurses-6.1", // TODO: why does psmisc not link against it?
		"containerd-1.2.0-beta.2",
		"docker-engine-18.06.1-ce",
		"docker-18.06.1-ce",
		// TODO: make these runtime deps of docker:
		"procps-ng-3.3.15",
		"iptables-1.6.2",
		"xzutils-5.2.4",
		"e2fsprogs-1.44.4",
		"kmod-25",
		// end of docker deps
		"runc-1.0.0-rc5",
		"grep-3.1",
		"openssh-7.8p1",
		"iproute2-4.18.0",
		"iputils-20180629",
		"linux-4.18.7",
		"ca-certificates-3.39",
		"grub2-2.02",
		// TODO: make these runtime deps of grub:
		"sed-4.5",
		"gawk-4.2.1",
		// end of grub deps
		"squashfs-4.3",
		"fuse-3.2.6",
		"haveged-1.9.4", // for gathering entropy on e.g. Google Cloud
		"dbus-1.13.6",
	})
	if err != nil {
		return fmt.Errorf("resolve: %v", err)
	}

	for _, pkg := range basePkgs {
		log.Printf("copying %s", pkg)
		if err := copyFile(filepath.Join(*imgDir, pkg+".squashfs"), filepath.Join(*root, "roimg", pkg+".squashfs")); err != nil {
			return err
		}
		if err := copyFile(filepath.Join(*imgDir, pkg+".meta.textproto"), filepath.Join(*root, "roimg", pkg+".meta.textproto")); err != nil {
			return err
		}

	}

	if _, err = mountfuse([]string{"-imgdir=" + filepath.Join(*root, "roimg"), filepath.Join(*root, "ro")}); err != nil {
		return err
	}
	defer fuse.Unmount(filepath.Join(*root, "ro"))

	// This is an initial installation of all packages, so copy their
	// /ro/<pkg>-<version>/etc directory contents to /etc (if any):
	for _, pkg := range basePkgs {
		pkgetc := filepath.Join(*root, "ro", pkg, "etc")
		if _, err := os.Stat(pkgetc); err != nil {
			continue // package has no etc directory
		}
		// TODO: do this copy in pure Go
		cp := exec.Command("cp", "--no-preserve=mode", "-r", pkgetc, *root)
		cp.Stderr = os.Stderr
		if err := cp.Run(); err != nil {
			return fmt.Errorf("%v: %v", cp.Args, err)
		}
	}

	// XXX: this is required for systemd-firstboot
	cmdline := filepath.Join(*root, "proc", "cmdline")
	if err := ioutil.WriteFile(cmdline, []byte("systemd.firstboot=1"), 0644); err != nil {
		return err
	}
	defer os.Remove(cmdline)
	cmd := exec.Command("unshare",
		"--user",
		"--map-root-user", // for mount permissions in the namespace
		"--mount",
		"--",
		"chroot", *root, "/ro/systemd-239/bin/systemd-firstboot", "--hostname=distri0",
		"--root-password=bleh",
		"--copy-timezone",
		"--copy-locale",
		"--setup-machine-id")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}

	cmd = exec.Command("unshare",
		"--user",
		"--map-root-user", // for mount permissions in the namespace
		"--mount",
		"--",
		"chroot", *root, "/ro/systemd-239/bin/systemd-sysusers",
		"/ro/systemd-239/buildoutput/lib/sysusers.d/basic.conf",
		"/ro/systemd-239/buildoutput/lib/sysusers.d/systemd.conf")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}

	cmd = exec.Command("unshare",
		"--user",
		"--map-root-user", // for mount permissions in the namespace
		"--mount",
		"--",
		"chroot", *root, "/ro/systemd-239/bin/systemctl",
		"enable",
		"systemd-networkd",
		"containerd",
		"docker",
		"ssh",
		"haveged")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}

	pamd := filepath.Join(*root, "etc", "pam.d")
	if err := os.MkdirAll(pamd, 0755); err != nil {
		return err
	}

	const pamdOther = `auth	required	pam_unix.so
auth	required	pam_warn.so
account	required	pam_unix.so
account	required	pam_warn.so
password	required	pam_deny.so
password	required	pam_warn.so
session	required	pam_unix.so
session	required	pam_warn.so
`
	if err := ioutil.WriteFile(filepath.Join(pamd, "other"), []byte(pamdOther), 0644); err != nil {
		return err
	}

	const dbusSystemLocal = `<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <includedir>/ro/share/dbus-1/system.d</includedir>
</busconfig>
`
	if err := ioutil.WriteFile(filepath.Join(*root, "etc", "dbus-1", "system-local.conf"), []byte(dbusSystemLocal), 0644); err != nil {
		return err
	}

	// TODO: implement adduser and addgroup function
	if err := adduser(*root, "systemd-network:x:101:101:network:/run/systemd/netif:/bin/false"); err != nil {
		return err
	}
	if err := addgroup(*root, "systemd-network:x:103:"); err != nil {
		return err
	}
	if err := adduser(*root, "systemd-resolve:x:105:105:resolve:/run/systemd/resolve:/bin/false"); err != nil {
		return err
	}
	if err := addgroup(*root, "systemd-resolve:x:105:"); err != nil {
		return err
	}

	if err := adduser(*root, "sshd:x:102:102:sshd:/:/bin/false"); err != nil {
		return err
	}

	if err := adduser(*root, "messagebus:x:106:106:messagebus:/var/run/dbus:/bin/false"); err != nil {
		return err
	}

	if err := addgroup(*root, "docker:x:104:"); err != nil {
		return err
	}

	if err := addgroup(*root, "messagebus:x:106:"); err != nil {
		return err
	}

	// TODO: once https://github.com/systemd/systemd/issues/3998 is fixed, use
	// their catch-all file rather than ours.
	network := filepath.Join(*root, "etc", "systemd", "network")
	if err := os.MkdirAll(network, 0755); err != nil {
		return err
	}

	if err := ioutil.WriteFile(filepath.Join(network, "ether.network"), []byte(`
[Match]
#Type=ether
Name=eth*

[Network]
DHCP=yes
`), 0644); err != nil {
		return err
	}

	modules := filepath.Join(*root, "etc", "modules-load.d")
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

	fuse.Unmount(filepath.Join(*root, "ro"))

	if *gcsDiskImg != "" && *diskImg == "" {
		// Creating a Google Cloud disk image requires creating a disk image
		// first, so use a temporary file:
		tmp, err := ioutil.TempFile("", "distriimg")
		if err != nil {
			return err
		}
		tmp.Close()
		defer os.Remove(tmp.Name())
		*diskImg = tmp.Name()
	}

	if *diskImg != "" {
		if err := writeDiskImg(*diskImg, *root); err != nil {
			return fmt.Errorf("writeDiskImg: %v", err)
		}
	}

	if *gcsDiskImg != "" {
		log.Printf("Writing Google Cloud disk image to %s", *gcsDiskImg)
		img, err := os.Open(*diskImg)
		if err != nil {
			return err
		}
		defer img.Close()
		st, err := img.Stat()
		if err != nil {
			return err
		}

		f, err := os.Create(*gcsDiskImg)
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

	return nil
}

func writeDiskImg(dest, src string) error {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_RDWR|unix.O_CLOEXEC, 0644)
	if err != nil {
		return err
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(4 * 1024 * 1024 * 1024); err != nil { // 4 GB
		return err
	}

	// Find the next free loop device:
	const (
		LOOP_CTL_GET_FREE = 0x4c82
		LOOP_SET_FD       = 0x4c00
		LOOP_SET_STATUS64 = 0x4c04
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
	loop, err := os.OpenFile(loopdev, os.O_RDWR|unix.O_CLOEXEC, 0644)
	if err != nil {
		return err
	}
	defer loop.Close()
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
		LO_FLAGS_READ_ONLY = 1
		LO_FLAGS_AUTOCLEAR = 4 // loop device will autodestruct on last close
	)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, loop.Fd(), LOOP_SET_FD, uintptr(f.Fd())); errno != 0 {
		return errno
	}
	var filename [64]byte
	copy(filename[:], []byte("root"))
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, loop.Fd(), LOOP_SET_STATUS64, uintptr(unsafe.Pointer(&LoopInfo64{
		flags:    LO_FLAGS_AUTOCLEAR | LO_FLAGS_READ_ONLY,
		filename: filename,
	}))); errno != 0 {
		return errno
	}

	sfdisk := exec.Command("sudo", "sfdisk", loopdev)
	sfdisk.Stdin = strings.NewReader(`size=250M, name=boot
name=root`)
	sfdisk.Stdout = os.Stdout
	sfdisk.Stderr = os.Stderr
	if err := sfdisk.Run(); err != nil {
		return fmt.Errorf("%v: %v", sfdisk.Args, err)
	}

	losetup := exec.Command("sudo", "losetup", "--show", "--find", "--partscan", dest)
	losetup.Stderr = os.Stderr
	out, err := losetup.Output()
	if err != nil {
		return fmt.Errorf("%v: %v", losetup.Args, err)
	}

	base := strings.TrimSpace(string(out))
	log.Printf("base: %q", base)

	boot := base + "p1"
	root := base + "p2"

	mkfs := exec.Command("sudo", "mkfs.ext2", boot)
	mkfs.Stdout = os.Stdout
	mkfs.Stderr = os.Stderr
	if err := mkfs.Run(); err != nil {
		return fmt.Errorf("%v: %v", mkfs.Args, err)
	}

	mkfs = exec.Command("sudo", "mkfs.ext4", root)
	mkfs.Stdout = os.Stdout
	mkfs.Stderr = os.Stderr
	if err := mkfs.Run(); err != nil {
		return fmt.Errorf("%v: %v", mkfs.Args, err)
	}

	if err := syscall.Mount(root, "/mnt", "ext4", syscall.MS_MGC_VAL, ""); err != nil {
		return fmt.Errorf("mount %s /mnt: %v", root, err)
	}
	defer syscall.Unmount("/mnt", 0)

	// TODO: get rid of this copying step
	cp := exec.Command("sudo", "sh", "-c", "cp -r "+filepath.Join(src, "*")+" /mnt/")
	cp.Stdout = os.Stdout
	cp.Stderr = os.Stderr
	if err := cp.Run(); err != nil {
		return fmt.Errorf("%v: %v", cp.Args, err)
	}

	if err := syscall.Mount(boot, "/mnt/boot", "ext2", syscall.MS_MGC_VAL, ""); err != nil {
		return fmt.Errorf("mount %s /mnt/boot: %v", boot, err)
	}
	defer syscall.Unmount("/mnt/boot", 0)

	if err := syscall.Mount("/dev", "/mnt/dev", "", syscall.MS_MGC_VAL|syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("mount /dev /mnt/dev: %v", err)
	}
	defer syscall.Unmount("/mnt/dev", 0)

	if err := syscall.Mount("/sys", "/mnt/sys", "", syscall.MS_MGC_VAL|syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("mount /sys /mnt/sys: %v", err)
	}
	defer syscall.Unmount("/mnt/sys", 0)

	chown := exec.Command("sudo", "chown", os.Getenv("USER"), "/mnt/ro")
	chown.Stderr = os.Stderr
	chown.Stdout = os.Stdout
	if err := chown.Run(); err != nil {
		return fmt.Errorf("%v: %v", chown.Args, err)
	}
	join, err := mountfuse([]string{"-imgdir=/mnt/roimg", "/mnt/ro"})
	if err != nil {
		return err
	}
	defer fuse.Unmount("/mnt/ro")

	if err := os.MkdirAll("/mnt/boot/grub", 0755); err != nil {
		return err
	}

	if err := copyFile(filepath.Join(distriRoot, "linux-4.18.7/arch/x86/boot/bzImage"), "/mnt/boot/vmlinuz-4.18.7"); err != nil {
		return err
	}

	mkconfig := exec.Command("sudo", "chroot", "/mnt", "sh", "-c", "GRUB_CMDLINE_LINUX=\"console=ttyS0,115200 root=/dev/sda2 init=/init rw\" GRUB_TERMINAL=serial grub-mkconfig -o /boot/grub/grub.cfg")
	mkconfig.Stderr = os.Stderr
	mkconfig.Stdout = os.Stdout
	if err := mkconfig.Run(); err != nil {
		return fmt.Errorf("%v: %v", mkconfig.Args, err)
	}

	install := exec.Command("sudo", "chroot", "/mnt", "grub-install", "--target=i386-pc", base)
	install.Stderr = os.Stderr
	install.Stdout = os.Stdout
	if err := install.Run(); err != nil {
		return fmt.Errorf("%v: %v", install.Args, err)
	}

	if err := fuse.Unmount("/mnt/ro"); err != nil {
		return fmt.Errorf("unmount /mnt/ro: %v", err)
	}

	if err := join(context.Background()); err != nil {
		return fmt.Errorf("fuse: %v", err)
	}

	chown = exec.Command("sudo", "chown", "root", "/mnt/ro")
	chown.Stderr = os.Stderr
	chown.Stdout = os.Stdout
	if err := chown.Run(); err != nil {
		return fmt.Errorf("%v: %v", chown.Args, err)
	}

	for _, m := range []string{"sys", "dev", "boot", ""} {
		if err := syscall.Unmount(filepath.Join("/mnt", m), 0); err != nil {
			return fmt.Errorf("unmount /mnt/%s: %v", m, err)
		}
	}

	losetup = exec.Command("sudo", "losetup", "-d", base)
	losetup.Stdout = os.Stdout
	losetup.Stderr = os.Stderr
	if err := losetup.Run(); err != nil {
		return fmt.Errorf("%v: %v", losetup.Args, err)
	}

	return nil
}

func adduser(root, line string) error {
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
