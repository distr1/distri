.PHONY: install

all: install

install:
# TODO: inherit CAP_SETFCAP
	CGO_ENABLED=0 go install ./cmd/distri && sudo setcap 'CAP_SYS_ADMIN,CAP_DAC_OVERRIDE=ep CAP_SETFCAP=eip' ~/go/bin/distri

test: install
	DISTRIROOT=$$PWD go test -v ./cmd/distri/... ./integration/...

image:
	sudo rm -rf /tmp/inst; DISTRIROOT=$$PWD distri pack -root=/tmp/inst -diskimg=/tmp/root.ext4

cryptimage:
	sudo rm -rf /tmp/inst; DISTRIROOT=$$PWD distri pack -root=/tmp/inst -diskimg=/tmp/root.ext4 -encrypt -base=base-x11 # -serialonly # for when you want to see non-kernel console output (e.g. systemd), useful with qemu

gcsimage:
	sudo rm -rf /tmp/inst; DISTRIROOT=$$PWD distri pack -root=/tmp/inst -diskimg=/tmp/root.ext4 -gcsdiskimg=/tmp/distri-gcs.tar.gz -base=base-cloud

qemu:
	qemu-system-x86_64 -device e1000,netdev=net0 -netdev user,id=net0,hostfwd=tcp::5555-:22 -device virtio-rng-pci -smp 8 -machine accel=kvm -m 4096 -kernel $$PWD/linux-4.18.7/arch/x86/boot/bzImage  -append "console=ttyS0,115200 root=/dev/sda4 rootfstype=ext4 init=/init rw" -nographic -drive if=none,id=hd,file=/tmp/root.ext4,format=raw -device virtio-scsi-pci,id=scsi -device scsi-hd,drive=hd

qemu-fastnet:
	qemu-system-x86_64 -device virtio-rng-pci -smp 8 -machine accel=kvm -m 4096 -kernel $$PWD/linux-4.18.7/arch/x86/boot/bzImage  -append "console=ttyS0,115200 root=/dev/sda4 rootfstype=ext4 init=/init rw" -nographic -drive if=none,id=hd,file=/tmp/root.ext4,format=raw -device virtio-scsi-pci,id=scsi -device scsi-hd,drive=hd

qemu-graphic:
	qemu-system-x86_64 -device e1000,netdev=net0 -netdev user,id=net0,hostfwd=tcp::5555-:22 -device virtio-rng-pci -smp 8 -machine accel=kvm -m 4096 -kernel $$PWD/linux-4.18.7/arch/x86/boot/bzImage  -append "root=/dev/sda4 rootfstype=ext4 init=/init rw" -drive if=none,id=hd,file=/tmp/root.ext4,format=raw -device virtio-scsi-pci,id=scsi -device scsi-hd,drive=hd

qemu-bios:
	qemu-system-x86_64 -device e1000,netdev=net0 -netdev user,id=net0,hostfwd=tcp::5555-:22 -device virtio-rng-pci -smp 8 -machine accel=kvm -m 4096 -drive if=none,id=hd,file=/tmp/root.ext4,format=raw -device virtio-scsi-pci,id=scsi -device scsi-hd,drive=hd -nographic

qemu-bios-graphic:
	qemu-system-x86_64 -device e1000,netdev=net0 -netdev user,id=net0,hostfwd=tcp::5555-:22 -device virtio-rng-pci -smp 8 -machine accel=kvm -m 4096 -drive if=none,id=hd,file=/tmp/root.ext4,format=raw -device virtio-scsi-pci,id=scsi -device scsi-hd,drive=hd
