# My preferred way to quickly test distri in a somewhat real environment is to
# use qemu (with KVM acceleration).
#
# To build an image in DISKIMG (default /tmp/root.img) which is ready to be used
# via qemu-serial¹, use e.g.:
#   % make image serial=1
#   % make qemu-serial
#
# If you want a graphical output instead, use e.g.:
#   % make image
#   % make qemu-graphic
#
# To test an encrypted root file system, substitute the image target with the
# cryptimage target.
#
# ① Unfortunately, the linux console can only print to one device.

DISKIMG=/tmp/distri-disk.img
GCEDISKIMG=/tmp/distri-gcs.tar.gz
DOCSDIR=/tmp/distri-docs

QEMU=qemu-system-x86_64 \
	-device e1000,netdev=net0 \
	-netdev user,id=net0,hostfwd=tcp::5555-:22 \
	-device virtio-rng-pci \
	-smp 8 \
	-machine accel=kvm \
	-m 4096 \
	-drive if=none,id=hd,file=${DISKIMG},format=raw \
	-device virtio-scsi-pci,id=scsi \
	-device scsi-hd,drive=hd

# To use gdb to debug the Linux kernel, use e.g.:
#
# make qemu-serial \
#   kgdb=1 \
#   kernel=/home/michael/fuse-debug/linux/arch/x86/boot/bzImage \
#   cmdline="panic_on_oops=1"
#
# gdb vmlinux
# (gdb) target remote localhost:1234
# (gdb) continue
ifdef kgdb
cmdline+= nokaslr
cmdline+= kgdbwait
cmdline+= kgdboc=ttyS0,115200
QEMU+= -serial tcp::1234,server,nowait
endif

ifdef kernel
QEMU+= -kernel "${kernel}"
QEMU+= -append "root=/dev/sda4 ro ${cmdline} $(shell tr -d '\n' < ${DISKIMG}.cmdline)"
endif

PACKFLAGS=

# for when you want to see non-kernel console output (e.g. systemd), useful with qemu
ifdef serial
PACKFLAGS+= -serialonly
endif

ifdef authorized_keys
PACKFLAGS+= -authorized_keys=${authorized_keys}
endif

ifdef branch
PACKFLAGS+= -branch=${branch}
endif

ifdef extra_kernel_params
PACKFLAGS+= -extra_kernel_params="${extra_kernel_params}"
endif

ifdef override_repo
PACKFLAGS+= -override_repo="${override_repo}"
endif

ifdef lvm
PACKFLAGS+= -lvm
endif

IMAGE=distri pack \
	-diskimg=${DISKIMG} \
	-base=base-x11 ${PACKFLAGS}

GCEIMAGE=distri pack \
	-gcsdiskimg=${GCEDISKIMG} \
	-base=base-cloud ${PACKFLAGS}

DOCKERTAR=distri pack -docker ${PACKFLAGS}

.PHONY: install

all: install

install:
# TODO: inherit CAP_SETFCAP
	CGO_ENABLED=0 go install ./cmd/... && sudo setcap 'CAP_SYS_ADMIN,CAP_DAC_OVERRIDE=ep CAP_SETFCAP=eip' $(shell go env GOPATH)/bin/distri
	# Enable using systemctl --user enable --now distri-autobuilder.service
	mkdir -p ~/.config/systemd/user && sed "s,@AUTOBUILDER@,$(shell which autobuilder),g" autobuilder.service.in > ~/.config/systemd/user/distri-autobuilder.service
	# This is a no-op if the unit is not running
	systemctl --user daemon-reload || true
	systemctl --user try-restart distri-autobuilder.service || true

test: install
	DISTRIROOT=$$PWD go test -failfast -v ./cmd/... ./integration/... ./internal/...

image:
	DISTRIROOT=$$PWD ${IMAGE}

cryptimage:
	DISTRIROOT=$$PWD ${IMAGE} -encrypt

gceimage:
	DISTRIROOT=$$PWD ${GCEIMAGE}

dockertar:
	@DISTRIROOT=$$PWD ${DOCKERTAR}

qemu-serial:
	${QEMU} -nographic

qemu-graphic:
	${QEMU}

.PHONY: docs screen usb release

docs: docs/building.asciidoc docs/package-format.asciidoc docs/index.asciidoc docs/rosetta-stone.asciidoc
	mkdir -p ${DOCSDIR}
	asciidoctor --destination-dir ${DOCSDIR} $^

# Example screen session for working with distri:
screen:
	# Start screen in detached mode
	screen -S distri -d -m
	# Window 1: make command to pack a new distri disk image
	screen -S distri -X title "image"
	screen -S distri -X stuff "make image serial=1 authorized_keys=~/.ssh/authorized_keys"
	# Window 2: make command to start the disk image in a qemu session
	screen -S distri -X screen
	screen -S distri -X title "qemu"
	screen -S distri -X stuff "make qemu-serial"
	# Window 3: SSH into the distri instance (better than serial)
	screen -S distri -X screen
	screen -S distri -X title "ssh"
	screen -S distri -X stuff "ssh distri0"
	# Setup done, now resume screen
	screen -r distri

usb:
	[ -n "${USB_DISK_ID}" ] || (echo "Usage example: make usb USB_DISK_ID=usb-SanDisk_Extreme_Pro_12345878D17B-0:0" >&2; false)
	sudo dd if=${DISKIMG} of=/dev/disk/by-id/${USB_DISK_ID} bs=1M status=progress oflag=direct

release:
	DISTRIROOT=$$PWD go run release/release.go

umount:
	for f in $$(mount | grep distri | cut -d' ' -f 3); do fusermount -u "$$f"; done
