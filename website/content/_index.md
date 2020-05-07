---
title: "distri: a Linux distribution to research fast package management"
menu:
  main:
    title: "Home"
    weight: 10
---

# distri

a Linux distribution to research fast package management

---

distri is a proof-of-concept implementation of the *simplest*¹ linux distribution I can think of that is still *useful*². Interestingly enough, in some cases the simple solution has inherent advantages, which I explore and contrast in the articles released at https://michael.stapelberg.ch/posts/tags/distri/

1. simple: while all the typical building blocks for a Linux distribution are present (a package builder, installer, tooling for creating patches, preparing package download mirrors, etc.), they all leave out many features. For example, the package format intentionally leaves out triggers and hooks, but can parallelize installation as a result.

1. useful: I have successfully booted and used distri images on qemu, Google Cloud, a Dell XPS 13 notebook. This includes booting from an encrypted root file system and running Google Chrome on Xorg to watch Netflix, which I consider a proxy for having a useful system.

Note that due to its research project status, it is **NOT RECOMMENDED** to use distri in ANY CAPACITY except for research. Specifically, do not expect any support.

distri is published in the hope that other, more established distributions, will find some parts of it interesting and decide to integrate those.

**For more details, please see my [blog article “introducing distri”](https://michael.stapelberg.ch/posts/2019-08-17-introducing-distri/)**. You can subscribe to all distri-related posts by subscribing to https://michael.stapelberg.ch/posts/tags/distri/feed.xml.

---

{{< getstarted >}}
