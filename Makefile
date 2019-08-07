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

IMAGE=distri pack \
	-diskimg=${DISKIMG} \
	-base=base-x11

GCEIMAGE=distri pack \
	-gcsdiskimg=${GCEDISKIMG} \
	-base=base-cloud

# for when you want to see non-kernel console output (e.g. systemd), useful with qemu
ifdef serial
IMAGE+= -serialonly
endif

.PHONY: install

all: install

install:
# TODO: inherit CAP_SETFCAP
	CGO_ENABLED=0 go install ./cmd/distri && sudo setcap 'CAP_SYS_ADMIN,CAP_DAC_OVERRIDE=ep CAP_SETFCAP=eip' ~/go/bin/distri

test: install
	DISTRIROOT=$$PWD go test -v ./cmd/... ./integration/...

image:
	DISTRIROOT=$$PWD ${IMAGE}

cryptimage:
	DISTRIROOT=$$PWD ${IMAGE} -encrypt

gceimage:
	DISTRIROOT=$$PWD ${GCEIMAGE}

qemu-serial:
	${QEMU} -nographic

qemu-graphic:
	${QEMU}

.PHONY: docs

docs: docs/building.asciidoc docs/package-format.asciidoc
	mkdir -p ${DOCSDIR}
	asciidoctor --destination-dir ${DOCSDIR} $^
