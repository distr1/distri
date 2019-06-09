package distri

import "testing"

func TestExtractPackageRevisionVersion(t *testing.T) {
	for _, tt := range []struct {
		filename string
		want     int64
	}{
		{
			filename: "less-amd64-530",
			want:     0,
		},

		{
			filename: "less-amd64-530-2",
			want:     2,
		},

		{
			filename: "less-amd64-530-17.squashfs.gz",
			want:     17,
		},

		{
			filename: "../less-amd64-530-17/bin/less", // exchange dir link target
			want:     17,
		},

		{
			filename: "build/git/build-2.9.5-3.log", // build log
			want:     3,
		},
	} {
		t.Run(tt.filename, func(t *testing.T) {
			got := extractPackageRevision(tt.filename)
			if got != tt.want {
				t.Fatalf("extractVersion(%v) = %v, want %v", tt.filename, got, tt.want)
			}
		})
	}
}
