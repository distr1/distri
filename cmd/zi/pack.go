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

func pack(args []string) error {
	fset := flag.NewFlagSet("pack", flag.ExitOnError)
	var (
		root = fset.String("root",
			"",
			"TODO")
		imgDir = fset.String("imgdir", filepath.Join(os.Getenv("HOME"), "zi/build/zi/pkg/"), "TODO")
		//pkg = fset.String("pkg", "", "path to .squashfs package to mount")
	)
	fset.Parse(args)
	if *root == "" {
		return fmt.Errorf("syntax: pack -root=<directory>")
	}

	for _, dir := range []string{
		"etc",
		"root",
		"dev",     // udev
		"ro",      // read-only package directory
		"proc",    // procfs
		"sys",     // sysfs
		"tmp",     // tmpfs
		"var/tmp", // systemd (e.g. systemd-networkd)
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

	basePkgs := []string{
		"systemd-239",
		// TODO: remove the following systemd deps:
		"libcap-2.25",
		"util-linux-2.32",
		// (end of systemd deps)
		// TODO: remove the following login deps:
		"pam-1.3.1",
		// (end of login deps)
		"glibc-2.27",
		"coreutils-8.30",
		"strace-4.24",
		"bash-4.4.18",
		"psmisc-23.1",
		"ncurses-6.1", // TODO: why does psmisc not link against it?
		"containerd-1.2.0-beta.2",
		"docker-engine-18.06.1-ce",
		"docker-18.06.1-ce",
		"procps-ng-3.3.15",
		"iptables-1.6.2",
		"xzutils-5.2.4",
		"e2fsprogs-1.44.4",
		"kmod-25",
		"runc-1.0.0-rc5",
		"grep-3.1",
		"openssh-7.8p1",
		// TODO: remove openssh deps:
		"openssl-1.0.2p",
		"zlib-1.2.11",
		// (end of login deps)
		"iproute2-4.18.0",
		// TODO: remove iproute2 deps:
		"libmnl-1.0.4",
		// (end of iproute2 deps)
		"iputils-20180629",
		// TODO: remove iputils deps:
		"libcap-2.25",
		"libidn2-2.0.5",
		"iconv-1.15",
		"libunistring-0.9.10",
		"nettle-3.4",
		// (end of iputils deps)
	}

	for _, pkg := range basePkgs {
		log.Printf("copying %s", pkg)
		if err := copyFile(filepath.Join(*imgDir, pkg+".squashfs"), filepath.Join(*root, "ro", pkg+".squashfs")); err != nil {
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
