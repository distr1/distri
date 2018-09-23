all:
	CGO_ENABLED=0 go install ./cmd/distri && sudo setcap CAP_SYS_ADMIN,CAP_DAC_OVERRIDE=ep ~/go/bin/distri

image:
	sudo rm -rf /tmp/inst; DISTRIROOT=$$PWD distri pack -root=/tmp/inst -diskimg=/tmp/root.ext4

qemu:
	qemu-system-x86_64 -device virtio-rng-pci -smp 8 -machine accel=kvm -m 1024 -kernel $$PWD/linux-4.18.7/arch/x86/boot/bzImage  -append "console=ttyS0,115200 root=/dev/vda2 rootfstype=ext4 init=/init rw" -nographic -drive format=raw,file=/tmp/root.ext4,if=virtio
