package main

import (
	"io/ioutil"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/env"
	"github.com/google/go-cmp/cmp"
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

func TestNewFile(t *testing.T) {
	c := scaffoldctx{
		ScaffoldType: scaffoldC,
		SourceURL:    "sourceurl",
		Name:         "distri-non-existant",
		Version:      "upstreamversion",
	}
	got := func() string {
		b, err := c.buildFile("hash")
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}()
	want := `source: "sourceurl"
hash: "hash"
version: "upstreamversion-1"

cbuilder: {}

# build dependencies:
`
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("scaffold: unexpected build.textproto file: diff (-want +got):\n%s", diff)
	}
}

func TestExistingFile(t *testing.T) {
	c := scaffoldctx{
		ScaffoldType: scaffoldC,
		SourceURL:    "sourceurl",
		Name:         "gcc",
		Version:      "upstreamversion",
	}
	got := func() string {
		b, err := c.buildFile("hash")
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}()
	want := func() string {
		b, err := ioutil.ReadFile(filepath.Join(env.DistriRoot, "pkgs", "gcc", "build.textproto"))
		if err != nil {
			t.Fatal(err)
		}
		want := string(b)
		want = regexp.MustCompile(`(?m)^source: "[^"]+"$`).
			ReplaceAllString(want, `source: "sourceurl"`)
		want = regexp.MustCompile(`(?m)^hash: "[^"]+"$`).
			ReplaceAllString(want, `hash: "hash"`)
		want = regexp.MustCompile(`(?m)^version: "[^"]+"$`).
			ReplaceAllStringFunc(want, func(in string) string {
				in = strings.TrimPrefix(in, `version: `)
				in, err = strconv.Unquote(in)
				if err != nil {
					t.Fatal(err)
				}
				pv := distri.ParseVersion(in)
				pv.DistriRevision++
				return `version: "upstreamversion-` + strconv.FormatInt(pv.DistriRevision, 10) + `"`
			})
		return want
	}()
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("scaffold: unexpected build.textproto file: diff (-want +got):\n%s", diff)
	}
}
