package build

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestPkgConfigFilesFromRequires(t *testing.T) {
	for _, test := range []struct {
		desc     string
		requires string
		want     []string
	}{
		{
			desc:     "names only",
			requires: "gdk cairo",
			want:     []string{"gdk", "cairo"},
		},

		{
			desc: "whitespace",
			requires: `gdk  cairo
atk`,
			want: []string{"gdk", "cairo", "atk"},
		},

		{
			desc:     "versions",
			requires: "gdk-3.0 atk >= 2.15.1 cairo",
			want: []string{
				"gdk-3.0",
				"atk",
				"cairo",
			},
		},

		{
			desc:     "versions and commas",
			requires: "gdk-3.0 atk >= 2.15.1 cairo,cairo-gobject",
			want: []string{
				"gdk-3.0",
				"atk",
				"cairo",
				"cairo-gobject",
			},
		},
	} {
		t.Run(test.desc, func(t *testing.T) {
			got := pkgConfigFilesFromRequires(test.requires)
			if diff := cmp.Diff(test.want, got); diff != "" {
				t.Errorf("pkgConfigFilesFromRequires(%q) = %q, want %q. diff: (-want +got):\n%s",
					test.requires, got, test.want, diff)
			}
		})
	}
}
