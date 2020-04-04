package build

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestNewerRevisionGoesFirst(t *testing.T) {
	// With this specific list of packages, a simple sort.SliceStable by distri
	// revision results in glibc-amd64-2.27 being placed _before_
	// glibc-amd64-2.31:
	deps := []string{
		"bash-amd64-5.0-4",
		"glibc-amd64-2.31-4",
		"ncurses-amd64-6.1-7",
		"coreutils-amd64-8.32-4",
		"attr-amd64-2.4.48-3",
		"gmp-amd64-6.2.0-4",
		"libcap-amd64-2.33-4",
		"sed-amd64-4.8-6",
		"grep-amd64-3.4-4",
		"gawk-amd64-5.0.1-4",
		"mpfr-amd64-4.0.2-4",
		"diffutils-amd64-3.7-3",
		"file-amd64-5.38-4",
		"zlib-amd64-1.2.11-3",
		"pkg-config-amd64-0.29.2-4",
		"glib-amd64-2.64.1-4",
		"libffi-amd64-3.3-4",
		"util-linux-amd64-2.32.1-7",
		"pam-amd64-1.3.1-10",
		"gcc-libs-amd64-9.3.0-4",
		"mpc-amd64-1.1.0-3",
		"make-amd64-4.3-4",
		"linux-amd64-5.5.2-12",
		"findutils-amd64-4.7.0-4",
		"musl-amd64-1.1.22-4",
		"strace-amd64-5.1-5",
		"glibc-amd64-2.27-3",
		"gcc-amd64-9.3.0-4",
		"binutils-amd64-2.34-4",
		"m4-amd64-1.4.18-3",
		"elfutils-amd64-0.179-4",
	}
	want := []string{
		"bash-amd64-5.0-4",
		"glibc-amd64-2.31-4",
		"glibc-amd64-2.27-3",
		"ncurses-amd64-6.1-7",
		"coreutils-amd64-8.32-4",
		"attr-amd64-2.4.48-3",
		"gmp-amd64-6.2.0-4",
		"libcap-amd64-2.33-4",
		"sed-amd64-4.8-6",
		"grep-amd64-3.4-4",
		"gawk-amd64-5.0.1-4",
		"mpfr-amd64-4.0.2-4",
		"diffutils-amd64-3.7-3",
		"file-amd64-5.38-4",
		"zlib-amd64-1.2.11-3",
		"pkg-config-amd64-0.29.2-4",
		"glib-amd64-2.64.1-4",
		"libffi-amd64-3.3-4",
		"util-linux-amd64-2.32.1-7",
		"pam-amd64-1.3.1-10",
		"gcc-libs-amd64-9.3.0-4",
		"mpc-amd64-1.1.0-3",
		"make-amd64-4.3-4",
		"linux-amd64-5.5.2-12",
		"findutils-amd64-4.7.0-4",
		"musl-amd64-1.1.22-4",
		"strace-amd64-5.1-5",
		"gcc-amd64-9.3.0-4",
		"binutils-amd64-2.34-4",
		"m4-amd64-1.4.18-3",
		"elfutils-amd64-0.179-4",
	}
	got := newerRevisionGoesFirst(deps)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("newerRevisionGoesFirst() returned unexpected order: diff (-want +got):\n%s", diff)
	}
}
