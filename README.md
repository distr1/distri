# distri — a Linux distribution to research fast package management

This repository contains distri, a linux distribution research project.

The contents form a proof-of-concept implementation of the simplest¹ linux distribution I can think of that is still useful². Interestingly enough, in some cases the simple solution has inherent advantages, which I explore and contrast in the articles released at https://michael.stapelberg.ch/posts/tags/distri/

1. simple: while all the typical building blocks for a Linux distribution are present (a package builder, installer, tooling for creating patches, preparing package download mirrors, etc.), they all leave out many features. For example, the package format intentionally leaves out triggers and hooks, but can parallelize installation as a result.

1. useful: I have successfully booted and used distri images on qemu, Google Cloud, a Dell XPS 13 notebook. This includes booting from an encrypted root file system and running Google Chrome on Xorg to watch Netflix, which I consider a proxy for having a useful system.

Note that due to its research project status, it is **NOT RECOMMENDED** to use distri in ANY CAPACITY except for research. Specifically, do not expect any support.

distri is published in the hope that other, more established distributions, will find some parts of it interesting and decide to integrate those.

**For more details, please see my [blog article “introducing distri”](https://michael.stapelberg.ch/posts/2019-08-02-introducing-distri/).** You can subscribe to all distri-related posts by subscribing to https://michael.stapelberg.ch/posts/tags/distri/feed.xml.

## Giving feedback

Please send feedback to the [distri mailing list](https://www.freelists.org/list/distri) so that everyone can participate!

You can also talk to us by connecting to https://robustirc.net/ and joining the `#distri` channel. Please stick around for a while, not everyone is at their keyboard all the time :)

## Getting started

Find current images of the `master` branch at https://repo.distr1.org/distri/master/img/.

With all images, use the `root` account, password `peace`, to log in.

### Run distri on real hardware

The easiest way to run distri on real hardware is to install it onto a spare USB memory stick.

Obtain a stable path to your USB memory stick by watching /dev/disk/by-id while inserting the stick:

```
% watch -dg ls '/dev/disk/by-id/*'
```

Then, copy the `distri-disk.img` image onto the memory stick:
```
dd if=distri-disk.img of=/dev/disk/by-id/usb-SanDisk_Extreme_Pro_D99B-0:0 bs=5M
```

Insert the memory stick into a computer and select the memory stick as boot device.

### Run distri in Docker

**NOTE**: There is a [known issue about intermittent FUSE issues](https://github.com/distr1/distri/issues/27) within Docker.

**NOTE**: As a heads-up, the docker [container image is pretty large](https://github.com/distr1/distri/issues/28)

1. (If you’d rather use a local docker container, build it locally: `distri pack -docker | docker import - distri`).
1. Then, run bash within the distri docker container:
```shell
docker run \
	--privileged \
	--entrypoint /entrypoint \
	-ti \
	-e TERM=$TERM \
	distr1/distri
```

### Run distri in qemu

Depending on what you want to test, the text-only serial interface might be a bit more convenient: it side-steps keyboard configuration mismatches and makes it easily to run distri remotely via an SSH session:

```shell
make qemu-serial DISKIMG=distri-qemu-serial.img
```
(You can exit by pressing `Ctrl-a x`)

If you want or need a graphical interface, use the `qemu-graphic` target with the standard `distri-disk.img` image:

```shell
make qemu-graphic DISKIMG=distri-disk.img
```

### Run distri in virtualbox

1. Convert the distri disk image into a VDI disk image so that virtualbox can use it as a root disk:

```shell
vbox-img convert \
	--srcfilename distri-disk.img \
	--dstfilename vbox-distri.vdi \
	--srcformat RAW \
	--dstformat VDI
```

1. Create a new VM:
    * click new button
    * select type linux
    * select version other linux (64-bit)
    * select the VDI disk image from step 1 as existing disk

### Run distri on Google Cloud

1. (If you’d rather use your own Google Cloud Storage bucket, import the `distri-gce.tar.gz` image into your Google Cloud Storage: `gsutil cp distri-gce.tar.gz gs://distri-gce`.)
1. Create a Compute Engine Image: `gcloud compute images create distri0 --source-uri gs://distri-gce/distri-gce.tar.gz`
1. Create VM using that image: `gcloud compute instances create instance-1 --zone us-east1-b --machine-type=f1-micro --image=distri0`
1. Log in via the serial console and set up an authorized SSH key.

## Cool things to try

### Fast package installation

<a href="https://asciinema.org/a/cwHaOq7LnY01lFB7kpQbAOVua" rel="nofollow"><img src="https://asciinema.org/a/cwHaOq7LnY01lFB7kpQbAOVua.svg" alt="asciicast" height=200 align="left"></a>

1. Verify `i3status` is not yet installed: `i3status --version`
1. Install the `i3status` package: `distri install i3status`
1. Verify `i3status` is now installed: `i3status --version`

<br clear="both" />

### Specific package versions

<a href="https://asciinema.org/a/VDKEQmsipIAy7e1FNTW3UbEt5" rel="nofollow"><img src="https://asciinema.org/a/VDKEQmsipIAy7e1FNTW3UbEt5.svg" alt="asciicast" height=200 align="left"></a>

1. Find out which package a file belongs to: `readlink -f /bin/i3`

1. If we are unhappy with the path that the exchange directory references, we can side-step it and make a specific selection:
```
% i3 --version
% /ro/i3-amd64-4.15*/bin/i3 --version
% /ro/i3-amd64-4.17*/bin/i3 --version
```

<br clear="both" />

<!--
TODO: https://asciinema.org/a/LtPyjOYazUYSOIj9AcguaPFRd
Look under the hood: wrapper programs
  % file /ro/git*/bin/git
  % readelf -p distrifilename !$
Include once the article about hermetic packages is done.
-->

### Exchange directories

<a href="https://asciinema.org/a/LFgF05pfvVwdIRghd19VTCXpB" rel="nofollow"><img src="https://asciinema.org/a/LFgF05pfvVwdIRghd19VTCXpB.svg" alt="asciicast" height=200 align="left"></a>

1. The `/bin` directory contains all executables: `ls /bin`
1. distri implements common file system hierarchy locations such as `/usr/include` as a symbolic link to an exchange directory:  `ls -l /usr/include`
1. Exchange directories consist of symbolic links to the files of individual distri packages: `ls -l /usr/include/`

<br clear="both" />

### C build environment

<a href="https://asciinema.org/a/LKvo6Ja8yUEvsVYJHMMeclIAq" rel="nofollow"><img src="https://asciinema.org/a/LKvo6Ja8yUEvsVYJHMMeclIAq.svg" alt="asciicast" height=200 align="left"></a>

1. Make available the build dependencies using `distri install autoconf automake make gcc libxcb xorgproto`
1. Build standard C software as usual:
```
% git clone https://github.com/i3/i3lock
% cd i3lock
% autoreconf -fi
% mkdir build && cd build
% ../configure
% make -j8
```

<br clear="both" />
