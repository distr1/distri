package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"text/template"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/checkupstream"
	"github.com/distr1/distri/internal/env"
	"github.com/google/go-cmp/cmp"
	"github.com/protocolbuffers/txtpbfmt/parser"
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

	again, err := c.buildFileExisting("<test>", "hash", []byte(got))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(got, string(again)); diff != "" {
		t.Fatalf("scaffold: unexpected build.textproto file: diff (-want +got):\n%s", diff)
	}
}

func TestScaffoldPullDebian(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `Package: google-chrome-stable
Version: 80.0.3987.106-1
Section: web
Filename: pool/main/g/google-chrome-stable/google-chrome-stable_80.0.3987.106-1_amd64.deb
SHA256: 33bdf0232923d4df0a720cce3a0c5a76eba15f88586255a91058d9e8ebf3a45d
Description: The web browser from Google
 Google Chrome is a browser that combines a minimal design with sophisticated technology to make the web faster, safer, and easier.

`)
	}))
	defer ts.Close()
	nodes, err := parser.Parse([]byte(fmt.Sprintf(`source: "%s/linux/chrome/deb/pool/main/g/google-chrome-stable/google-chrome-stable_80.0.3987.87-1_amd64.deb"
pull: {
  debian_packages: "%s/linux/chrome/deb/dists/stable/main/binary-amd64/Packages"
}
version: "80.0.3987.106-1-13"
`, ts.URL, ts.URL)))
	if err != nil {
		t.Fatal(err)
	}

	remoteSource, remoteHash, remoteVersion, err := checkupstream.Check(nodes)
	if err != nil {
		t.Fatal(err)
	}

	wantSource := ts.URL + "/linux/chrome/deb/pool/main/g/google-chrome-stable/google-chrome-stable_80.0.3987.106-1_amd64.deb"
	wantHash := "33bdf0232923d4df0a720cce3a0c5a76eba15f88586255a91058d9e8ebf3a45d"
	wantVersion := "80.0.3987.106-1"
	if got, want := remoteSource, wantSource; got != want {
		t.Errorf("scaffoldPullDebian: got source %q, want %q", got, want)
	}
	if got, want := remoteHash, wantHash; got != want {
		t.Errorf("scaffoldPullDebian: got hash %q, want %q", got, want)
	}
	if got, want := remoteVersion, wantVersion; got != want {
		t.Errorf("scaffoldPullDebian: got version %q, want %q", got, want)
	}
}

var buildFileTmpl = template.Must(template.New("").Parse(`# leading comment
source: "{{ .Source }}"
hash: "{{ .Hash }}"
version: "{{ .Version }}"
pull: {
  debian_packages: "{{ .DebianPackages }}"
}

cbuilder: {}
`))

func TestScaffoldPull(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `Package: google-chrome-stable
Version: 80.0.3987.106-1
Section: web
Filename: pool/main/g/google-chrome-stable/google-chrome-stable_80.0.3987.106-1_amd64.deb
SHA256: 33bdf0232923d4df0a720cce3a0c5a76eba15f88586255a91058d9e8ebf3a45d
Description: The web browser from Google
 Google Chrome is a browser that combines a minimal design with sophisticated technology to make the web faster, safer, and easier.

`)
	}))
	defer ts.Close()
	packagesURL := ts.URL + "/linux/chrome/deb/dists/stable/main/binary-amd64/Packages"

	f, err := ioutil.TempFile("", "distri-scaffold-test")
	if err != nil {
		t.Fatal(err)
	}

	type tmplData struct {
		Source         string
		Hash           string
		Version        string
		DebianPackages string
	}
	old := func() string {
		var buf bytes.Buffer
		if err := buildFileTmpl.Execute(&buf, tmplData{
			Source:         "http://dl.google.com/linux/chrome/deb/pool/main/g/google-chrome-stable/google-chrome-stable_80.0.3987.87-1_amd64.deb",
			Hash:           "85e07dee624d3c7eec6a6194efcb070b353ee52c0e5980517760230128a3ba61",
			Version:        "80.0.3987.87-1-11",
			DebianPackages: packagesURL,
		}); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}()
	f.Write([]byte(old))
	f.Close()
	defer os.Remove(f.Name())
	buildFilePath := f.Name()
	if err := scaffoldPull("google-chrome", buildFilePath, false); err != nil {
		t.Fatal(err)
	}

	b, err := ioutil.ReadFile(buildFilePath)
	if err != nil {
		t.Fatal(err)
	}

	if string(b) == old {
		t.Fatalf("scaffoldPull: build.textproto unexpectedly unchanged")
	}
	want := func() string {
		var buf bytes.Buffer
		if err := buildFileTmpl.Execute(&buf, tmplData{
			Source:         "http://dl.google.com/linux/chrome/deb/pool/main/g/google-chrome-stable/google-chrome-stable_80.0.3987.106-1_amd64.deb",
			Hash:           "33bdf0232923d4df0a720cce3a0c5a76eba15f88586255a91058d9e8ebf3a45d",
			Version:        "80.0.3987.106-1-12",
			DebianPackages: packagesURL,
		}); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}()
	if diff := cmp.Diff(want, string(b)); diff != "" {
		t.Errorf("scaffoldPull: unexpected contents: diff (-want +got):\n%s", diff)
	}
}
