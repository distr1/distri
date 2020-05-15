---
title: "Release Notes: distri supersilverhaze (2020-05-16)"
date: 2020-05-15 08:00:00 +02:00
---

# distri supersilverhaze (2020-05-16)

If you’re not familiar, distri is a Linux distribution to research fast package management.

You can read more about distri starting in my [introduction blog post](https://michael.stapelberg.ch/posts/2019-08-17-introducing-distri/), then in my [series of blog posts about distri](https://michael.stapelberg.ch/posts/tags/distri/).

Please direct any questions or feedback to the [distri mailing list](https://www.freelists.org/list/distri).

---

<form>
<div class="form-group d-flex justify-content-end">
{{< getstarted >}}
{{< thingstotry >}}
</div>
</form>

---

## Changes since the last release

The following sections briefly cover new features in distri, with links to more
details.

If you’re curious, you can also browse the [commits since the last
release](https://github.com/distr1/distri/compare/jackherer...supersilverhaze).

### minitrd

Building a distri disk image (using e.g. `make image` or `distri pack` directly)
is now significantly faster, because the most time-consuming step has been
replaced: initrds are no longer built with `dracut`, but with [`minitrd`, a custom
from-scratch implementation optimized for
speed](https://michael.stapelberg.ch/posts/2020-01-21-initramfs-from-scratch-golang/).

Whereas it previously took ≈40 seconds, a **full distri disk image can now be
generated in 4 seconds** (with warm caches)!

### Debugging

<a href="/things-to-try/"><img src="/img/gdb-debugging-session.thumb.jpg" srcset="/img/gdb-debugging-session.thumb.2x.jpg 2x,/img/gdb-debugging-session.thumb.3x.jpg 3x" width="200" align="right" style="border: 1px solid grey"></a>

I have previously written [about the debugging experience in
Debian](https://michael.stapelberg.ch/posts/2019-02-15-debian-debugging-devex/),
which left a lot to be desired. This release of distri comes with two features
that result in what I consider the gold standard of debugging experience.

#### Debugging: package sources

All package sources are now available under `/usr/src/`,
e.g. `/usr/src/procps-ng-amd64-3.3.15-8/`. This is powered by:

1. The `srcfs` service, which (like `debugfs` for `/ro-dbg`) is a `distri fuse`
   daemon which lazily downloads packages from the repository when they are
   accessed.

2. `distri build` now writes a source squashfs images. Files which are
   referenced by debug info and cannot be found in the source directory will be
   taken from the build directory. This makes generated files available for
   debugging at source level.

Caveat: 15 packages contain at least one generated source code file which is unavailable, to be fixed at a later date.

#### Debugging: DWARF debug symbols

The [DWARF](https://en.wikipedia.org/wiki/DWARF) debug symbols of all packages
are available through `/ro-dbg`,
e.g. `/ro-dbg/emacs-amd64-26.3-15/debug/.build-id/d8/149e8d4e11dc451ac5e718dfc44344c610e23e.debug`. While
the backing `debugfs` service was already included in the previous release, its
reliability has been improved: `distri fuse -autodownload` now keeps retrying
when started without network connectivity.

Caveat: 16 packages do not currently ship debug symbols, even though they
should. A number of other packages do not ship debug symbols because they do not
contain [ELF files](https://en.wikipedia.org/wiki/Executable_and_Linkable_Format).

### FUSE performance

* `distri fuse`: avoid running out of file descriptors with many packages by [increasing the open file limit](https://github.com/distr1/distri/commit/af8455d3b396a2d97a07dc05ddcf6ec0439ae062)
* `distri fuse`: [significant optimizations in scanPackages (now >3x as fast)](https://github.com/distr1/distri/pull/58)
* [Cache file lookups resulting in `-ENOENT`](https://github.com/distr1/distri/commit/b6a0e43368d54d5ed0e03af687158dc3e2106e38). This speeds up dynamic library loading when starting programs.
* [issue #59](https://github.com/distr1/distri/issues/59) contains an overview of FUSE optimizations that were applied and are yet to be explored.

<a href="https://twitter.com/zekjur/status/1226615325682262017"><img src="/img/chrome-tracing-profile.thumb.jpg" srcset="/img/chrome-tracing-profile.thumb.2x.jpg 2x,/img/chrome-tracing-profile.thumb.3x.jpg 3x" width="200" align="right" style="border: 1px solid grey"></a>

### Package build performance

Improving the package build performance results in faster batch builds, and a
better interactive development experience when iterating on a single package.

* `make` 4.3 fixes performance issues with heavily concurrent builds, e.g. `linux` (down from over an hour to merely 10 minutes).
* `distri batch` and `distri build` now write [a chrome://tracing profile by default](https://twitter.com/zekjur/status/1226615325682262017) (including CPU user/sys counters and MemAvailable counter)
* The FUSE performance optimizations listed in the previous section significantly speed up package building, too. E.g. `gtk+-2` now builds twice as fast (≈2 minutes down to ≈1 minute).
* see https://github.com/distr1/distri/issues/59 for details
* use `${DISTRI_JOBS}` instead of hard-coded `-j8`, add `distri build -jobs` flag
* `distri build`: ninja: explicitly specify -j, ninja falls back to 3 without the `/proc` file system

### txtpbfmt

We now use the [`txtpbfmt` Go package (and
program)](https://github.com/protocolbuffers/txtpbfmt) to enforce consistent
formatting of `build.textproto` files across the repository.

Furthermore, `txtpbfmt` allows for programmatic modification of `build.textproto` files. The `distri scaffold` subcommand uses `txtpbfmt` to only update the upstream `url`, `hash` and `version` fields when the package `build.textproto` file already exists. In particular, this means that manual additions (including comments!) are preserved.

To make upgrading packages to more recent upstream versions even more convenient, the `distri scaffold` subcommand now has a `-pull` flag which will heuristically (see repobrowser for more details) pull in a new upstream version:

```shell
distri scaffold -pull google-chrome
2020/05/12 09:23:25 not up to date: updating from 80.0.3987.106-1 to 81.0.4044.138-1
```

```patch
diff --git i/pkgs/google-chrome/build.textproto w/pkgs/google-chrome/build.textproto
index 91e4113..589300a 100644
--- i/pkgs/google-chrome/build.textproto
+++ w/pkgs/google-chrome/build.textproto
@@ -1,6 +1,6 @@
-source: "http://dl.google.com/linux/chrome/deb/pool/main/g/google-chrome-stable/google-chrome-stable_80.0.3987.106-1_amd64.deb"
-hash: "33bdf0232923d4df0a720cce3a0c5a76eba15f88586255a91058d9e8ebf3a45d"
-version: "80.0.3987.106-1-15"
+source: "http://dl.google.com/linux/chrome/deb/pool/main/g/google-chrome-stable/google-chrome-stable_81.0.4044.138-1_amd64.deb"
+hash: "9d13d41d79ce1f04d1f150b5d22fffd31779224cc7d8274f8479b06bcfe6846a"
+version: "81.0.4044.138-1-16"
 pull: {
   debian_packages: "https://dl.google.com/linux/chrome/deb/dists/stable/main/binary-amd64/Packages"
 }
```

### repobrowser

The distri repo browser is now available at
[https://browse.distr1.org](https://browse.distr1.org/)!

Similar to https://godoc.org/, the distri repo browser can visualize the
contents of any distri repository. The public instance at
https://browse.distr1.org is restricted to repositories hosted on
https://repo.distr1.org for the time being.

The repo browser displays not only the current versions of all packages in the
repository, but also flags packages as out of date in case a more recent
upstream version is known.

#### repobrowser: upstream check heuristic

Checking upstream for newer versions is done largely
[heuristically](https://github.com/distr1/distri/commit/08073898f33e12d9bec824bb3bd29581efe3dd94). For
a few popular services (GitHub, GitLab, Go modules, etc.), service-specific APIs
are used. More specific code can be added where required, but thus far, the
heuristic covers the vast majority of packages.

34 packages (6.2%) are currently not categorized because the upstream check
failed.

For some packages, this is expected: for example `autoconf2.13` is technically
out of date (`autoconf` 2.69 is current), but `mozjs` specifically requires the
older 2.13, so we will need to keep that outdated package around.

In other cases, upstream is unreachable. Either temporarily (server issues, like
recently with freedesktop.org) or permanently, e.g. `giblib`, where the upstream
URL results in a connection refused error for years.

Sometimes the heuristic results in a false-positive result: the package is shown
as up-to-date, but a new major version is available in a different
subdirectory. Some projects (e.g. `util-linux`) publish release within a major
version in their own subdirectory,
e.g. https://mirrors.edge.kernel.org/pub/linux/utils/util-linux/v2.32/util-linux-2.32.1.tar.xz.

The heuristic should be changed to detect that situation, which is tracked in
[issue #68](https://github.com/distr1/distri/issues/68).

### Tab completion

Tab completion for package installation is now available by default:

Press the tab key after entering `distri install ` and you will be presented
with a list of available packages.

This is powered by `distri list`, which fetches the repository metadata on demand.

Side note: the default shell for the `root` account was switched from `bash` to
`zsh` for this, which also enables history by default.

### New packages

A few new packages have been added, typically because I needed them on my test machine:

bluez,
dnsmasq,
dunst,
encfs,
intel-ucode,
linux-cpupower,
mesa-demos,
nano,
pigz,
screen,
tmux,
upower,
vim,
wget,
xclip,
xwininfo

### New upstream versions

A number of packages has been updated to their current upstream versions:

* 432 packages (79.56%) are up to date
* 68 packages (12.52%) have a newer upstream available
  * 43 packages are go modules with a newer minor version
  * 25 packages are not go modules, but upstream packages where something blocks us from updating, e.g. new bugs or build failures with downstreams

### Hygiene

* All `distri(1)` commands now handle interruptions (`SIGINT`, `SIGTERM`) and clean up temporary files.
* `distri build` now [actually mounts the source directory read-only](https://github.com/distr1/distri/commit/a0041ffcb523ad6f4cab2c2002594c25d02cdffb) ([Linux ignores the `MS_RDONLY` flag for bind mounts](https://unix.stackexchange.com/a/128388/181634)).
* `distri patch` now provides a proper interactive shell.
* `distri build` now logs the whole build output (including distri messages), not just the build steps.
* `distri build`’s `-debug` flag now accepts the name of a stage when to spawn a debug shell, e.g. `after-install`.

### Microcode

GRUB 2.04 now applies CPU microcode updates for Intel CPUs at early boot (`amd-ucode` is just not yet packaged).

### Makefile

* [The `kgdb=` and `kernel=` parameters allow easier kernel debugging](https://github.com/distr1/distri/commit/fd798e188a673884d50b8f96e17a7204b8ae2c3d)
* The `screen` target starts a development `screen(1)` session
* The `usb` target writes the distri image to e.g. a USB stick:
```shell
make usb USB_DISK_ID=usb-SanDisk_Extreme_Pro_12345678D99C-0:0
```

### autobuilder

More work went into the autobuilder, but it is not deployed yet.

The autobuilder should eventually build every git commit, so that we have quick,
automated feedback regarding possible breakages, and an easy way to test the
current version of distri, whether released or not.

Due to the large data volume (dozens of gigabytes per commit) and operational
challenges (building a single commit from scratch takes ≈2 hours on an Intel
Core i9-9900K), this setup requires careful tweaking.

## Update instructions

To set expectations: due to its experimental nature, distri does not make any
guarantees that updates will work. We try to make them work, as long as you
follow the update instructions described here. Good luck! :)

### Ensuring enough free disk space

The `jackherer` disk image is 7 GB in size, which is not sufficient to hold the default installation in both versions (temporarily). Double the image in size on the host:
```shell
truncate -s +$((7*1024*1024*1024)) distri-disk.img
```
…and enlarge the file systems within the image:
```shell
distri install golang && go get github.com/google/embiggen-disk && ~/go/bin/embiggen-disk -verbose /
```

### Consider switching from dracut to minitrd

To switch from dracut to minitrd, use:

```shell
echo minitrd > /etc/distri/initramfs-generator
```

This will be effective upon the next installation of the `linux` package, including the one in the `distri update` step below.

### Updating packages

Switch to the new release’s package repository:

```shell
echo https://repo.distr1.org/distri/supersilverhaze > /etc/distri/repos.d/distr1.repo
```

**Workaround:** Update the `distri1` package, which contains the `distri(1)`
command, before any others:

```shell
distri install distri1
```

This is required for the next command to work properly, and should have been an
automatic step. Unfortunately, the previously released version has a bug: it
[accidentally ran the same old version again instead of the newly installed
one](https://github.com/distr1/distri/commit/76f76f80efbe0aa07e8277470ad1e4fe5ec581ef).


```shell
distri update
```

Because `systemctl` fully resolves symlinks when creating symbolic links in
`/etc/systemd/system` to enable a unit, the old version of e.g. SSH remains
enabled, and the new version explicitly needs to be enabled (improvements tracked in [issue
#69](https://github.com/distr1/distri/issues/69)):

```shell
systemctl --root=/ disable ssh
systemctl --root=/ enable ssh
```

Reboot into the new system to pick up all new package versions:

```shell
reboot
```

If you want, you can delete the old packages from your package store:

```shell
distri gc
```

…but you can also keep them so that you can use older versions to reproduce or
work around a bug.
