package main

import (
	"net/url"
	"testing"
)

func mustParse(u string) *url.URL {
	parsed, err := url.Parse(u)
	if err != nil {
		panic(err)
	}
	return parsed
}

func TestNameFromURL(t *testing.T) {
	for _, tt := range []struct {
		URL          *url.URL
		scaffoldType int
		wantName     string
		wantVersion  string
	}{
		{
			URL:          mustParse("http://ftp.debian.org/debian/pool/main/w/whois/whois_5.4.2.tar.xz"),
			scaffoldType: scaffoldC,
			wantName:     "whois",
			wantVersion:  "5.4.2",
		},

		{
			URL:          mustParse("https://xorg.freedesktop.org/releases/individual/lib/libXxf86vm-1.1.4.tar.bz2"),
			scaffoldType: scaffoldC,
			wantName:     "libxxf86vm",
			wantVersion:  "1.1.4",
		},

		{
			URL:          mustParse("https://github.com/traviscross/mtr/archive/v0.92.tar.gz"),
			scaffoldType: scaffoldC,
			wantName:     "mtr",
			wantVersion:  "0.92",
		},
	} {
		t.Run(tt.URL.String(), func(t *testing.T) {
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
