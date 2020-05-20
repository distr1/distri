---
title: "distri: mirrors"
---

# Mirrors

As a user of distri, please use the default repository at https://repo.distr1.org/.

If you are interested in contributing to the project by providing a mirror
server, the rest of this document is for you!

## distri

The distri research linux distribution project was started in 2019 to research
whether a few architectural changes could enable drastically faster package
management.

While the package managers in common Linux distributions (e.g. apt, dnf, …) [top
out at data rates of only a few
MB/s](https://michael.stapelberg.ch/posts/2019-08-17-linux-package-managers-are-slow/),
distri effortlessly saturates 1 Gbit, 10 Gbit and even 40 Gbit connections,
resulting in superior installation and update speeds.

You can read more about distri starting in my [introduction blog post](https://michael.stapelberg.ch/posts/2019-08-17-introducing-distri/), then in my [series of blog posts about distri](https://michael.stapelberg.ch/posts/tags/distri/).

## Why run a mirror?

Currently, the distri project only has servers in Europe, so our geographic
proximity to the rest of the world is pretty poor. See the First Byte results as
reported on
[https://performance.sucuri.net/domain/repo.distr1.org](https://performance.sucuri.net/domain/repo.distr1.org):

<img src="/img/ttfb.jpg" width="600" alt="time to first byte results" style="border: 1px solid #ccc">

---

If you are interested in contributing to the project and have the continued
means to run a mirror, preferably in areas not yet well-covered, that would be
much appreciated! Users in more parts of the world will be able to experience
how fast package management can be, and your mirror can play a big role in that!

## Mirror requirements

* Please connect your mirror to the internet with a connection speed of **at least 1 Gbit/s**.

* Please allocate **at least 150 GiB** of disk space for the distri mirror: the
[two most recent distri releases](https://distr1.org/release-notes/) use 66 GiB
and 26 GiB, respectively. We want to be able to serve at least the two most
recent distri release from all mirrors.

* Please **synchronize your mirror at least every 24 hours** with the distri
  repository at https://repo.distr1.org/, e.g. by using the following `rsync`
  invocation in a daily cronjob:

  ```shell
  rsync -av rsync://repo-rsync.distr1.org/distri /srv/repo.distr1.org/distri
  ```
  
  Note: repo-rsync.distr1.org only allows access for explicitly allowed IPv4 or
  IPv6 addresses or ranges. Please get in touch if you are interested to run a
  mirror! Reach out to [Michael Stapelberg](https://michael.stapelberg.ch/) via
  email.

## Any questions?

Please get in touch! Reach out to [Michael
Stapelberg](https://michael.stapelberg.ch/) via email.
