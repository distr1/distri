package main

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

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
		imgDir  = fset.String("imgdir", filepath.Join(os.Getenv("HOME"), "zi/build/zi/pkg/"), "TODO")
		diskImg = fset.String("diskimg", "", "Write an ext4 file system image to the specified path")
		//pkg = fset.String("pkg", "", "path to .squashfs package to mount")
	)
	fset.Parse(args)
	if *root == "" {
		return fmt.Errorf("syntax: pack -root=<directory>")
	}

	for _, dir := range []string{
		"etc",
		"root",
		"dev",         // udev
		"ro",          // read-only package directory
		"proc",        // procfs
		"sys",         // sysfs
		"tmp",         // tmpfs
		"var/tmp",     // systemd (e.g. systemd-networkd)
		"lib/systemd", // systemd
	} {
		if err := os.MkdirAll(filepath.Join(*root, dir), 0755); err != nil {
			return err
		}
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

	if err := os.Symlink("/ro/system", filepath.Join(*root, "lib", "systemd", "system")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := os.Symlink("/ro/linux-4.18.7/buildoutput/lib/modules", filepath.Join(*root, "lib", "modules")); err != nil && !os.IsExist(err) {
		return err
	}

	// TODO: de-duplicate with zi.go
	if err := os.Symlink("/ro/glibc-2.27/buildoutput/lib", filepath.Join(*root, "lib64")); err != nil && !os.IsExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Join(*root, "ro", "bin"), 0755); err != nil {
		return err
	}
	if err := os.Symlink("/ro/bin/bash", filepath.Join(*root, "ro", "bin", "sh")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := copyFile("/proc/self/exe", filepath.Join(*root, "init")); err != nil {
		return err
	}

	if err := os.Chmod(filepath.Join(*root, "init"), 0755); err != nil {
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
	})
	if err != nil {
		return fmt.Errorf("resolve: %v", err)
	}

	for _, pkg := range basePkgs {
		log.Printf("copying %s", pkg)
		if err := copyFile(filepath.Join(*imgDir, pkg+".squashfs"), filepath.Join(*root, "ro", pkg+".squashfs")); err != nil {
			return err
		}
		if err := copyFile(filepath.Join(*imgDir, pkg+".meta.textproto"), filepath.Join(*root, "ro", pkg+".meta.textproto")); err != nil {
			return err
		}
	}

	for _, pkg := range basePkgs {
		cleanup, err := mount([]string{"-root=" + filepath.Join(*root, "ro"), pkg})
		if err != nil {
			return err
		}
		defer cleanup()
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
		"chroot", *root, "/ro/systemd-239/buildoutput/bin/systemd-firstboot", "--hostname=zi0",
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
		"chroot", *root, "/ro/systemd-239/buildoutput/bin/systemd-sysusers",
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
		"chroot", *root, "/ro/systemd-239/buildoutput/bin/systemctl",
		"enable",
		"systemd-networkd")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}

	pamd := filepath.Join(*root, "etc", "pam.d")
	if err := os.MkdirAll(pamd, 0755); err != nil {
		return err
	}

	const pamdLogin = `account  sufficient  pam_permit.so
auth  sufficient  pam_permit.so
`

	if err := ioutil.WriteFile(filepath.Join(pamd, "login"), []byte(pamdLogin), 0644); err != nil {
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

	// TODO: implement adduser and addgroup function
	if err := adduser(*root, "systemd-network:x:101:101:network:/run/systemd/netif:/bin/false"); err != nil {
		return err
	}
	if err := addgroup(*root, "systemd-network:x:103:"); err != nil {
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

	if *diskImg != "" {
		if err := writeDiskImg(*diskImg, *root); err != nil {
			return fmt.Errorf("writeDiskImg: %v", err)
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
	if err := f.Truncate(2 * 1024 * 1024 * 1024); err != nil { // 2 GB
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

	mkfs := exec.Command("sudo", "mkfs.ext4", loopdev)
	mkfs.Stdout = os.Stdout
	mkfs.Stderr = os.Stderr
	if err := mkfs.Run(); err != nil {
		return fmt.Errorf("%v: %v", mkfs.Args, err)
	}

	if err := syscall.Mount(loopdev, "/mnt", "ext4", syscall.MS_MGC_VAL, ""); err != nil {
		return err
	}

	cp := exec.Command("sudo", "sh", "-c", "cp -r "+filepath.Join(src, "*")+" /mnt/")
	cp.Stdout = os.Stdout
	cp.Stderr = os.Stderr
	if err := cp.Run(); err != nil {
		syscall.Unmount("/mnt", 0)
		return fmt.Errorf("%v: %v", cp.Args, err)
	}

	if err := syscall.Unmount("/mnt", 0); err != nil {
		return err
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
