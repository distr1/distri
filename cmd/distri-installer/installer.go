// Program distri-installer installs distri on a block device, i.e. delete all
// data and replace its contents with distri.
//
// Example usage:
//    distri-installer -override_block_device=/dev/sdx
package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/env"
)

type installctx struct {
	source               string
	overwriteBlockDevice string
	runningDistri        bool
	dryRun               bool
	lvm                  bool
	encrypt              bool
	extraLVMHook         string
	serialOnly           bool
	repo                 string
}

func (i *installctx) install(ctx context.Context) error {
	// TODO(later): refactor pack so that we don’t need to re-exec and duplicate
	// flags:

	start := time.Now()

	// We re-use distri pack because it only writes what is necessary. This is
	// unlike copying partitions by copying their raw bytes, which is simpler
	// but significantly slower, as it writes a lot more bytes.
	pack := exec.CommandContext(ctx,
		"distri",
		"pack",
		"-base=base-x11")
	if i.lvm {
		pack.Args = append(pack.Args, "-lvm")
	}
	if i.serialOnly {
		pack.Args = append(pack.Args, "-serialonly")
	}
	if i.encrypt {
		pack.Args = append(pack.Args, "-encrypt")
	}
	if i.extraLVMHook != "" {
		pack.Args = append(pack.Args, "-extra_lvm_hook="+i.extraLVMHook)
	}
	if i.overwriteBlockDevice != "" {
		pack.Args = append(pack.Args, "-overwrite_block_device="+i.overwriteBlockDevice)
	}
	if i.repo != "" {
		pack.Args = append(pack.Args, "-repo="+i.repo)
	}

	pack.Stdout = os.Stdout
	pack.Stderr = os.Stderr
	if i.dryRun {
		log.Printf("%v", pack.Args)
	} else {
		log.Printf("%v", pack.Args)
		if err := pack.Run(); err != nil {
			return fmt.Errorf("%v: %v", pack.Args, err)
		}
	}

	log.Printf("install succeded in %v", time.Since(start))

	return nil
}

func rootBlockdevOfRunningSystem(fn string) string {
	b, err := ioutil.ReadFile(fn)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		dev, mountpoint := fields[0], fields[1]
		if mountpoint != "/" {
			continue
		}
		return dev
	}
	return ""
}

func rootBlockdevOfRunningDistri(fn string) string {
	dev := rootBlockdevOfRunningSystem(fn)
	if dev == "" {
		return ""
	}

	// Verify that this is distri by checking the partition label
	blkid := exec.Command("blkid", dev)
	blkid.Stderr = os.Stderr
	b, err := blkid.Output()
	if err != nil {
		return ""
	}
	rootLabelFound := false
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) == "PARTLABEL=root" {
			rootLabelFound = true
			break
		}
	}
	if !rootLabelFound {
		return ""
	}

	if strings.HasSuffix(dev, "p4") {
		return strings.TrimSuffix(dev, "p4") // e.g. /dev/nvme0n1p4
	}
	return strings.TrimSuffix(dev, "4") // e.g. /dev/sda4

}

func defaultRepo() string {
	if _, err := os.Stat("/roimg"); err == nil {
		return "/roimg" // running on distri
	}
	return env.DefaultRepo // requires a distri checkout
}

func installer() error {
	rootBlockdevOfRunningDistri := rootBlockdevOfRunningDistri("/proc/self/mounts")
	var (
		overwriteBlockDevice = flag.String(
			"overwrite_block_device",
			"",
			"path to a block device (e.g. /dev/sda or /dev/nvme0n1) which to OVERWRITE, i.e. delete all data and replace its contents with distri")

		source = flag.String(
			"source",
			rootBlockdevOfRunningDistri,
			"distri disk image to install to disk (can be the running system)")

		lvm = flag.Bool(
			"lvm",
			false,
			"place the root file system on an LVM logical volume")

		encrypt = flag.Bool(
			"encrypt",
			false,
			"Whether to encrypt the image’s partitions (with LUKS)")

		dryRun = flag.Bool(
			"dry_run",
			false,
			"print commands instead of writing data")

		serialOnly = flag.Bool(
			"serialonly",
			false,
			"Whether to print output only on console=ttyS0,115200 (defaults to false, i.e. console=tty1)")

		extraLVMHook = flag.String(
			"extra_lvm_hook",
			"",
			"path to an executable program that modifies the LVM setup after the distri installer created it")

		repo = flag.String(
			"repo",
			defaultRepo(),
			"distri pkg/ repo dir from which to install packages")
	)
	flag.Parse()

	// if *source == "" {
	// 	return fmt.Errorf("specifying a distri disk image to install from via the -source flag is required when not running on distri")
	// }

	if *overwriteBlockDevice == "" {
		return fmt.Errorf("-overwrite_block_device must be specified")
	}

	i := &installctx{
		source:               *source,
		overwriteBlockDevice: *overwriteBlockDevice,
		runningDistri: rootBlockdevOfRunningDistri != "" &&
			*source == rootBlockdevOfRunningDistri,
		dryRun:       *dryRun,
		lvm:          *lvm,
		encrypt:      *encrypt,
		extraLVMHook: *extraLVMHook,
		serialOnly:   *serialOnly,
		repo:         *repo,
	}
	ctx, canc := distri.InterruptibleContext()
	defer canc()
	return i.install(ctx)
}

func main() {
	if err := installer(); err != nil {
		log.Fatal(err)
	}
}
