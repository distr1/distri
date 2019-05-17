package main

import "testing"

func TestNameFromURL(t *testing.T) {
	for _, tt := range []struct {
		URL          string
		scaffoldType int
		wantName     string
		wantVersion  string
	}{
		{
			URL:          "http://ftp.debian.org/debian/pool/main/w/whois/whois_5.4.2.tar.xz",
			scaffoldType: scaffoldC,
			wantName:     "whois",
			wantVersion:  "5.4.2",
		},

		{
			URL:          "https://xorg.freedesktop.org/releases/individual/lib/libXxf86vm-1.1.4.tar.bz2",
			scaffoldType: scaffoldC,
			wantName:     "libxxf86vm",
			wantVersion:  "1.1.4",
		},

		{
			URL:          "https://github.com/traviscross/mtr/archive/v0.92.tar.gz",
			scaffoldType: scaffoldC,
			wantName:     "mtr",
			wantVersion:  "0.92",
		},
	} {
		t.Run(tt.URL, func(t *testing.T) {
			name, version, err := nameFromURL(tt.URL, tt.scaffoldType)
			if err != nil {
				t.Fatalf("nameFromURL: %v", err)
			}
			if got, want := name, tt.wantName; got != want {
				t.Errorf("unexpected name for %s: got %q, want %q", tt.URL, got, want)
			}
			if got, want := version, tt.wantVersion; got != want {
				t.Errorf("unexpected version for %s: got %q, want %q", tt.URL, got, want)
			}
		})
	}
}
