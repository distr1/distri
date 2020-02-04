package distri

import "testing"

func TestExtractPackageRevisionVersion(t *testing.T) {
	for _, tt := range []struct {
		filename string
		want     PackageVersion
	}{
		{
			filename: "less-amd64-530",
			want:     PackageVersion{Pkg: "less", Arch: "amd64", Upstream: "530", DistriRevision: 0},
		},

		{
			filename: "530",
			want:     PackageVersion{Upstream: "530", DistriRevision: 0},
		},

		{
			filename: "530-3",
			want:     PackageVersion{Upstream: "530", DistriRevision: 3},
		},

		{
			filename: "v0.0.0-20180314180146-1d60e4601c6f",
			want:     PackageVersion{Upstream: "v0.0.0-20180314180146-1d60e4601c6f"},
		},

		{
			filename: "gcc-i686-amd64-8.2.0-3.squashfs",
			want:     PackageVersion{Pkg: "gcc-i686", Arch: "amd64", Upstream: "8.2.0", DistriRevision: 3},
		},

		{
			filename: "gcc-i686-amd64-8.2.0.squashfs",
			want:     PackageVersion{Pkg: "gcc-i686", Arch: "amd64", Upstream: "8.2.0", DistriRevision: 0},
		},

		{
			filename: "glibc-i686-host-amd64-2.27-3",
			want:     PackageVersion{Pkg: "glibc-i686-host", Arch: "amd64", Upstream: "2.27", DistriRevision: 3},
		},

		{
			filename: "less-amd64-530-2",
			want:     PackageVersion{Pkg: "less", Arch: "amd64", Upstream: "530", DistriRevision: 2},
		},

		{
			filename: "less-amd64-530-17.squashfs.gz",
			want:     PackageVersion{Pkg: "less", Arch: "amd64", Upstream: "530", DistriRevision: 17},
		},

		{
			filename: "../less-amd64-530-17/bin/less", // exchange dir link target
			want:     PackageVersion{Pkg: "less", Arch: "amd64", Upstream: "530", DistriRevision: 17},
		},

		{
			filename: "../libxslt-amd64-1.1.32-1/bin/xslt-config",
			want:     PackageVersion{Pkg: "libxslt", Arch: "amd64", Upstream: "1.1.32", DistriRevision: 1},
		},

		{
			filename: "build/git/build-2.9.5-3.log", // build log
			want:     PackageVersion{Upstream: "2.9.5", DistriRevision: 3},
		},

		{
			filename: "build/git/build-2.9.5.log", // build log
			want:     PackageVersion{Upstream: "2.9.5", DistriRevision: 0},
		},

		{
			filename: "../../../linux-amd64-4.18.7/out/lib/modules/4.18.7/build",
			want:     PackageVersion{Pkg: "linux", Arch: "amd64", Upstream: "4.18.7", DistriRevision: 0},
		},
	} {
		t.Run(tt.filename, func(t *testing.T) {
			got := ParseVersion(tt.filename)
			if got != tt.want {
				t.Fatalf("extractVersion(%v) = %#v, want %#v", tt.filename, got, tt.want)
			}
		})
	}
}

func TestPackageRevisionLess(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want bool
	}{
		{
			a:    "../libxslt-amd64-1.1.32/bin/xslt-config",
			b:    "../libxslt-amd64-1.1.32-1/bin/xslt-config",
			want: true,
		},
	} {
		t.Run(tt.a+"_"+tt.b, func(t *testing.T) {
			got := PackageRevisionLess(tt.a, tt.b)
			if got != tt.want {
				t.Logf("ParseVersion(%s) = %v", tt.a, ParseVersion(tt.a))
				t.Logf("ParseVersion(%s) = %v", tt.b, ParseVersion(tt.b))
				t.Errorf("PackageRevisionLess(%s, %s) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
