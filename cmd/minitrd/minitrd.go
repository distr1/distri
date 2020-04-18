// Program minitrd is a minimal init program to be used in a Linux initramfs. It
// loads kernel modules, opens LUKS-encrypted block devices, mounts the root
// file system and proceeds with booting.
//
// The following kernel cmdline parameters are respected:
//
//    - rd.luks=1 to enable looking for LUKS-encrypted block devices
//    - rd.luks.uuid=<uuid>
//    - rd.luks.name=<uuid>=<name>
//    - root=/dev/mapper/<name>
//    - root=UUID=<uuid>
//    - rootfstype=<fs>, e.g. rootfstype=ext4
//    - rd.vconsole.font=<font>
//
// The following files are expected to be included in the initramfs:
//
//    - /etc/localtime for timestamps in the correct time zone
//    - /lib/modules/<uname -r> (kernel modules)
//    - /cryptsetup for LUKS
package main

// CGO_ENABLED=0 GOFLAGS=-ldflags=-w go install

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/s-urbaniak/uevent"
	"golang.org/x/sync/errgroup"
)

const debug = false

func mount(source, target, fstype string) error {
	if err := os.MkdirAll(target, 0755); err != nil {
		return err
	}

	if err := syscall.Mount(source, target, fstype, 0, ""); err != nil {
		if sce, ok := err.(syscall.Errno); ok && sce == syscall.EBUSY {
			// /sys was already mounted
		} else {
			return fmt.Errorf("%v: %v", target, err)
		}
	}

	return nil
}

var cmdline = make(map[string]string)

func parseCmdline() error {
	b, err := ioutil.ReadFile("/proc/cmdline")
	if err != nil {
		return err
	}
	parts := strings.Split(strings.TrimSpace(string(b)), " ")
	for _, part := range parts {
		// separate key/value based on the first = character;
		// there may be multiple (e.g. in rd.luks.name)
		if idx := strings.IndexByte(part, '='); idx > -1 {
			cmdline[part[:idx]] = part[idx+1:]
		} else {
			cmdline[part] = ""
		}
	}
	return nil
}

// execCommand is a wrapper around exec.Command which sets up the environment
// and stdin/stdout/stderr.
func execCommand(name string, arg ...string) error {
	cmd := exec.Command(name, arg...)
	// For locating the GCC runtime library (libgcc_s.so.1):
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH=/ro/lib:/ro/lib64")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %v", cmd.Args, err)
	}
	return nil
}

func luksOpen(dev, name string) error {
	return execCommand("/cryptsetup", "luksOpen", dev, name)
}

// There is a great explanation of the chdir/mount/chroot dance at
// https://github.com/mirror/busybox/blob/9ec836c033fc6e55e80f3309b3e05acdf09bb297/util-linux/switch_root.c#L297
func switchRoot() error {
	// mount devtmpfs on /mnt/dev so that distri init has /dev/null
	if err := mount("dev", "/mnt/dev", "devtmpfs"); err != nil {
		return err
	}

	// mount proc on /mnt/proc so that distri init can increase RLIMIT_NOFILE
	if err := mount("proc", "/mnt/proc", "proc"); err != nil {
		return err
	}

	// TODO(later): remove files from initramfs to free up RAM

	if err := os.Chdir("/mnt"); err != nil {
		return err
	}

	if err := syscall.Mount(".", "/", "", syscall.MS_MOVE, ""); err != nil {
		return fmt.Errorf("mount . /: %v", err)
	}

	if err := syscall.Chroot("."); err != nil {
		return fmt.Errorf("chroot .: %v", err)
	}

	if err := os.Chdir("/"); err != nil {
		return err
	}

	return syscall.Exec("/init", []string{"/init"}, os.Environ())
}

func rootFS(path string, r io.Closer) error {
	rootfstype := cmdline["rootfstype"]
	if rootfstype == "" {
		// TODO(later): probe for the root fs type
		rootfstype = "ext4"
	}
	log.Printf("minitrd: mounting root file system %s (%v)", path, rootfstype)
	if err := syscall.Mount(path, "/mnt", rootfstype, 0, ""); err != nil {
		return fmt.Errorf("mount root file system: %v", err)
	}
	// We need to close our uevent connection, otherwise udev won’t
	// function correctly.
	r.Close() // TODO(upstream): change upstream to use syscall.CloseOnExec instead
	if err := switchRoot(); err != nil {
		return fmt.Errorf("switch_root: %v", err)
	}
	return nil
}

// pollName repeatedly tries to read a file until it appears.
// This is required because the dm/name file within /sys might not
// exist yet when we receive the uevent about the new dm device.
func pollName(path string) (string, error) {
	const timeout = 5 * time.Second
	start := time.Now()
	for time.Since(start) < timeout {
		dmName, err := ioutil.ReadFile(path)
		if err == nil {
			return string(dmName), nil
		}
		if err != nil {
			if os.IsNotExist(err) {
				time.Sleep(1 * time.Millisecond)
				continue
			}
			return "", err
		}
	}
	return "", fmt.Errorf("%v did not appear within %v", path, timeout)
}

// devAdd is called upon receiving a uevent from the kernel with action “add”
// from subsystem “block”.
func devAdd(devpath, devname string, start time.Time, r io.Closer) error {
	if strings.HasPrefix(devname, "dm-") {
		dmName, err := pollName(filepath.Join("/sys", devpath, "dm/name"))
		if err != nil {
			return err
		}
		dmPath := "/dev/mapper/" + strings.TrimSpace(dmName)
		if got, want := dmPath, cmdline["root"]; got == want {
			return rootFS(dmPath, r)
		} else {
			log.Printf("minitrd: ignoring %s, looking for root=%s", got, want)
			// fall-through so that we can open e.g. LUKS volumes on top of LVM:
		}
	}

	if strings.HasPrefix(devpath, "/devices/platform/floppy") {
		return nil // skip floppy devices to avoid an error message (no medium)
	}

	f, err := os.Open(filepath.Join("/dev", devname))
	if err != nil {
		return fmt.Errorf("uevent: %v", err)
	}
	isLVM := probeLVM(f) == nil
	uuid, err := blkid(f)
	f.Close()
	if isLVM {
		if err := execCommand("/vgchange", "-ay"); err != nil {
			return err
		}
		if err := execCommand("/vgmknodes"); err != nil {
			return err
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("blkid(%v): %v", devname, err)
	}
	if cmdline["rd.luks"] == "1" {
		if got, want := uuid, cmdline["rd.luks.uuid"]; got != want {
			return fmt.Errorf("LUKS: skipping block device %v (uuid %v), looking for uuid %v", devname, got, want)
		}

		parts := strings.Split(cmdline["rd.luks.name"], "=")
		if len(parts) != 2 {
			return fmt.Errorf("rd.luks.name kernel parameter malformed (expected rd.luks.name=<uuid>=<name>)")
		}
		log.Printf("minitrd: root block device /dev/%v appeared in %v", devname, time.Since(start))
		if err := luksOpen(filepath.Join("/dev", devname), parts[1]); err != nil {
			return fmt.Errorf("cryptsetup luksOpen: %v", err)
		}
	} else {
		want := strings.TrimPrefix(cmdline["root"], "UUID=")
		if got := uuid; got != want {
			return fmt.Errorf("skipping block device %v (uuid %v), looking for uuid %v", devname, got, want)
		}
		log.Printf("minitrd: root block device /dev/%v appeared in %v", devname, time.Since(start))
		return rootFS("/dev/"+devname, r)
	}

	return nil
}

func setFont() error {
	font, ok := cmdline["rd.vconsole.font"]
	if !ok {
		return nil // keep default consolefont
	}
	log.Printf("minitrd: setting console font %v", font)
	return execCommand("/setfont", font)
}

func setKeymap() error {
	keymap, ok := cmdline["rd.vconsole.keymap"]
	if !ok {
		return nil // keep default keymap
	}
	log.Printf("minitrd: setting keymap %v", keymap)
	return execCommand("/loadkeys", keymap)
}

func skipDeviceMapper(dmCookie string) bool {
	if dmCookie == "" {
		return false // device not set up by libdevmapper
	}

	// skip device mapper devices if their cookie has flag
	// DM_UDEV_DISABLE_DISK_RULES_FLAG set:
	// https://sourceware.org/git/?p=lvm2.git;a=blob;f=lib/activate/dev_manager.c;hb=d9e8895a96539d75166c0f74e58f5ed4e729e551#l1935
	cookie, err := strconv.ParseUint(dmCookie, 0, 32)
	if err != nil {
		return false // invalid cookie
	}
	// libdevmapper.h
	const (
		DM_UDEV_FLAGS_SHIFT             = 16
		DM_UDEV_DISABLE_DISK_RULES_FLAG = 0x0004
	)
	flags := cookie >> DM_UDEV_FLAGS_SHIFT
	return flags&DM_UDEV_DISABLE_DISK_RULES_FLAG > 0
}

func logic() error {
	start := time.Now()
	var eg errgroup.Group
	eg.Go(parseAliases)
	eg.Go(parseDeps)
	eg.Go(func() error { return mount("dev", "/dev", "devtmpfs") })
	eg.Go(func() error { return mount("sys", "/sys", "sysfs") })
	eg.Go(func() error { return mount("proc", "/proc", "proc") })
	// Creating /run/cryptsetup avoids a cryptsetup warning message
	eg.Go(func() error { return os.MkdirAll("/run/cryptsetup", 0755) })
	eg.Go(func() error { return os.MkdirAll("/mnt", 0755) })
	if err := eg.Wait(); err != nil {
		return err
	}
	// TODO(upstream): should cryptsetup load this module automatically?
	// it definitely should not just exit 1 without any messages
	if err := loadModule("algif_skcipher"); err != nil {
		return err
	}
	// TODO(upstream): lvm2’s vgchange should load the kernel modules for
	// individual RAID personalities automatically where required. It currently
	// seems to depend on the initramfs loading the personalities, or the
	// personalities being compiled not as a module (but as part of dm-raid).
	for _, personality := range []string{
		"linear",
		"multipath",
		"raid0",
		"raid1",
		"raid456",
		"raid10",
	} {
		// TODO(optimization): load these in parallel
		if err := loadModule(personality); err != nil {
			return err
		}
	}
	if err := parseCmdline(); err != nil {
		return err
	}

	go func() {
		if err := setFont(); err != nil {
			log.Printf("minitrd: setting console font failed: %v", err)
			// not a fatal error, keep booting
		}
	}()

	go func() {
		if err := setKeymap(); err != nil {
			log.Printf("minitrd: setting keymap failed: %v", err)
			// not a fatal error, keep booting
		}
	}()

	var (
		work      = make(chan string, 10)
		modSeenMu sync.Mutex
		modSeen   = make(map[string]bool)
	)
	seen := func(modalias string) bool {
		modSeenMu.Lock()
		defer modSeenMu.Unlock()
		if modSeen[modalias] {
			return true
		}
		modSeen[modalias] = true
		return false
	}
	// Loading modules in parallel shaves off 100ms of boot-to-blockdev time
	// (from 300ms total).
	for i := 0; i < runtime.NumCPU(); i++ {
		go func() {
			for modalias := range work {
				if seen(modalias) {
					continue
				}
				if err := loadModalias(modalias); err != nil {
					log.Printf("minitrd: loadModalias: %v", err)
				}
			}
		}()
	}

	// Subscribe to kernel uevent messages to get notifications about:
	// - new block devices (to luksOpen maybe)
	// - new devices with MODALIAS variables (more module loading)
	r, err := uevent.NewReader()
	if err != nil {
		return err
	}
	dec := uevent.NewDecoder(r)
	go func() {
		for {
			ev, err := dec.Decode()
			if err != nil {
				log.Printf("minitrd: uevent: %v", err)
				os.Exit(1)
			}
			if modalias, ok := ev.Vars["MODALIAS"]; ok {
				if err := loadModalias(modalias); err != nil {
					log.Printf("minitrd: loadModalias: %v", err)
				}
			}
			devname, ok := ev.Vars["DEVNAME"]
			if !ok {
				continue // unexpected uevent message
			}
			// The libdevmapper activation sequence results in an add uevent
			// before the device is ready, so wait for the change uevent:
			// https://www.redhat.com/archives/linux-lvm/2020-April/msg00004.html
			if !(((!strings.HasPrefix(devname, "dm-") && ev.Action == "add") ||
				(strings.HasPrefix(devname, "dm-") && ev.Action == "change")) &&
				ev.Subsystem == "block") {
				continue
			}
			if skipDeviceMapper(ev.Vars["DM_COOKIE"]) {
				log.Printf("skipping device mapper device %s because of DM_COOKIE", devname)
				continue
			}
			if err := devAdd(ev.Devpath, devname, start, r); err != nil {
				log.Printf("minitrd: %v", err)
				continue
			}
		}
	}()

	// Identify existing block devices (e.g. NVMe disks may already be
	// enumerated before the initrd even starts):
	go func() {
		err := filepath.Walk("/sys/block", func(path string, info os.FileInfo, err error) error {
			if err != nil {
				log.Printf("minitrd: %v", err)
				return nil
			}
			if strings.HasPrefix(path, "/sys/block/loop") {
				return nil
			}
			if info.Mode()&os.ModeSymlink == 0 {
				return nil
			}
			target, err := filepath.EvalSymlinks(path)
			if err != nil {
				log.Printf("minitrd: %v", err)
				return nil
			}
			devname := filepath.Base(path)
			go func() {
				if err := devAdd(target, devname, start, r); err != nil {
					log.Printf("minitrd: %v", err)
				}
			}()
			// Probe all partitions of this block device, too:
			fis, err := ioutil.ReadDir(target)
			if err != nil {
				log.Printf("minitrd: %v", err)
				return nil
			}
			for _, fi := range fis {
				if !strings.HasPrefix(fi.Name(), devname) {
					continue
				}
				go func(devname string) {
					devpath := filepath.Join(target, devname)
					if err := devAdd(devpath, devname, start, r); err != nil {
						log.Printf("minitrd: %v", err)
					}
				}(fi.Name())
			}
			return nil
		})
		if err != nil {
			log.Printf("minitrd: %v", err)
		}
	}()

	err = filepath.Walk("/sys", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Printf("minitrd: %v", err)
			return nil
		}
		if info == nil {
			return fmt.Errorf("nil info")
		}
		if info.Name() != "modalias" {
			return nil
		}
		eg.Go(func() error {
			b, err := ioutil.ReadFile(path)
			if err != nil {
				return err
			}
			if modalias := strings.TrimSpace(string(b)); modalias != "" {
				work <- modalias
			}
			return nil
		})
		return nil
	})
	if err != nil {
		return err
	}
	if err := eg.Wait(); err != nil {
		return err
	}
	close(work)
	// block indefinitely: once the root file system appears, we will switchRoot
	select {}
}

func main() {
	if err := logic(); err != nil {
		log.Fatalf("minitrd: %v", err)
	}
}
