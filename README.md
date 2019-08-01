# distri — a linux research project

This repository contains distri, a linux distribution research project.

The contents form a proof-of-concept implementation of the simplest¹ linux
distribution I can think of that is still useful². Interestingly enough, in some
cases the simple solution has inherent advantages, which I explore and contrast
in the articles released at https://michael.stapelberg.ch/posts/tags/distri/

① simple: while all the typical building blocks for a Linux distribution are
  present (a package builder, installer, tooling for creating patches, preparing
  package download mirrors, etc.), they all leave out many features. For
  example, the package format intentionally leaves out triggers and hooks, but
  can parallelize installation as a result.

② useful: I have successfully booted and used distri images on qemu, Google
  Cloud, a Dell XPS 13 notebook. This includes booting from an encrypted root
  file system and running Google Chrome on Xorg, which I consider a proxy for
  having a useful system.

Note that due to its research project status, it is NOT RECOMMENDED to use
distri in ANY CAPACITY except for research. Specifically, do not expect any
support.

distri is published in the hope that other, more established distributions, will
find some parts of it interesting and decide to integrate those.
