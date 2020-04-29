package checkupstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/distr1/distri"
	"github.com/protocolbuffers/txtpbfmt/ast"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/net/html"
)

const hashFromDownload = "" // sentinel

// CheckResult encapsulates the result of an upstream check, i.e. the latest
// remote version.
type CheckResult struct {
	Source  string
	Hash    string
	Version string
}

type check struct {
	// config
	source             string // TODO: maybe retire this field in favor of sourceURL?
	sourceURL          *url.URL
	sourceForgeProject string // extracted from source
	pv                 distri.PackageVersion
	rePatternExpr      string
	replaceAllExpr     string
	replaceAllRepl     string
	forceSemver        bool
}

func (c *check) SourceURL() *url.URL {
	clone := *c.sourceURL
	return &clone
}

func (c *check) checkDebian(debianPackagesURL string) (*CheckResult, error) {
	ctx, canc := context.WithTimeout(context.Background(), 5*time.Second)
	defer canc()
	req, err := http.NewRequestWithContext(ctx, "GET", debianPackagesURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		return nil, fmt.Errorf("%v: HTTP %v", debianPackagesURL, resp.Status)
	}
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(b), "\n")
	sourceUrl := c.SourceURL()
	sourceUrl.Path = path.Dir(sourceUrl.Path)
	var filename, remoteVersion, remoteHash string
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "Filename: "):
			filename = strings.TrimPrefix(line, "Filename: ")
			continue

		case strings.HasPrefix(line, "Version: "):
			remoteVersion = strings.TrimPrefix(line, "Version: ")
			continue

		case strings.HasPrefix(line, "SHA256: "):
			remoteHash = strings.TrimPrefix(line, "SHA256: ")
			continue

		case strings.TrimSpace(line) == "":
			// package stanza done

		default:
			continue
		}

		if !strings.HasSuffix(sourceUrl.Path, path.Dir(filename)) {
			continue
		}
		remoteBase := path.Base(filename)
		sourceUrl.Path = path.Join(sourceUrl.Path, remoteBase)
		return &CheckResult{
			Source:  sourceUrl.String(),
			Hash:    remoteHash,
			Version: remoteVersion,
		}, nil
	}
	return nil, fmt.Errorf("package not found in Debian Packages file")
}

func maybeV(v string) string {
	if strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

func extractLinks(parent *url.URL, b []byte) ([]string, error) {
	doc, err := html.Parse(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	var links []string
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			var href string
			for _, attr := range n.Attr {
				if attr.Key != "href" {
					continue
				}
				href = attr.Val
				break
			}
			if href != "" {
				if uri, err := url.Parse(href); err == nil {
					links = append(links, parent.ResolveReference(uri).String())
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(doc)
	return links, nil
}

func (c *check) extractVersions(b string) ([]string, error) {
	pattern := c.rePatternExpr
	if pattern == "" {
		// TODO: add special casing for parsing apache directory index?
		base := path.Base(c.source)
		log.Printf("base: %v", base)
		upstreamVersion := c.pv.Upstream
		idx := strings.Index(base, upstreamVersion)
		if idx == -1 {
			idx = strings.Index(base, strings.ReplaceAll(upstreamVersion, ".", "_"))
		}
		if idx == -1 {
			return nil, fmt.Errorf("upstreamVersion %q not found in base %q", upstreamVersion, base)
		}
		pattern = regexp.QuoteMeta(base[:idx]) + `([0-9vBp._-]*)` + regexp.QuoteMeta(base[idx+len(upstreamVersion):])
	}

	if pattern == path.Base(c.source) {
		return nil, fmt.Errorf("could not derive regexp pattern, specify manually")
	}
	log.Printf("pattern: %v", pattern)
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("compile(pattern): %v", err)
	}
	log.Printf("re: %v", re)
	matches := re.FindAllStringSubmatch(string(b), -1)
	log.Printf("matches: %v", matches)
	if c.replaceAllExpr != "" {
		re, err := regexp.Compile(c.replaceAllExpr)
		if err != nil {
			return nil, fmt.Errorf("compile(replaceAllExpr): %v", err)
		}
		filtered := make([][]string, len(matches))
		for idx, match := range matches {
			filtered[idx] = []string{"", re.ReplaceAllString(match[1], c.replaceAllRepl)}
		}
		matches = filtered
		log.Printf("filtered: %v", filtered)
	}
	versions := func() []string {
		v := make(map[string]bool)
		for _, m := range matches {
			if m[1] == "latest" {
				continue // skip common latest symlink
			}
			v[m[1]] = true
		}
		result := make([]string, 0, len(v))
		for version := range v {
			result = append(result, version)
		}
		valid := true
		if c.forceSemver {
			filtered := make([]string, 0, len(result))
			for _, r := range result {
				if !semver.IsValid(maybeV(r)) {
					continue
				}
				filtered = append(filtered, r)
			}
			result = filtered
		} else {
			for _, r := range result {
				if !semver.IsValid(maybeV(r)) {
					log.Printf("not semver: %v", r)
					valid = false
					break
				}
			}
		}
		if !valid {
			// Prefer a string sort when the versions aren’t semver, it’s better
			// than semver.Compare.
			sort.Sort(sort.Reverse(sort.StringSlice(result)))
		} else {
			sort.Slice(result, func(i, j int) bool {
				v, w := result[i], result[j]
				v, w = maybeV(v), maybeV(w)
				return semver.Compare(v, w) >= 0 // reverse
			})
		}
		return result
	}()
	return versions, nil
}

// TODO: signature is getting long. move to a struct
func (c *check) checkHeuristic(releasesURL string) (*CheckResult, error) {
	u, _ := url.Parse(releasesURL)
	log.Printf("fetching releases from %s", u.String())
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/80.0.3987.87 Safari/537.36")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP response: got %v, want 200 OK", resp.Status)
	}
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	links, err := extractLinks(u, b)
	if err != nil {
		return nil, err
	}

	upstreamVersion := c.pv.Upstream
	versions, err := c.extractVersions(string(b))
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("not yet implemented")
	}

	newBase := strings.Replace(path.Base(c.source), upstreamVersion, versions[0], 1)
	log.Printf("looking for <a> with href=%s", newBase)
	for _, l := range links {
		if filepath.Base(l) == newBase {
			log.Printf("found link: %s", l)
			return &CheckResult{
				Source:  l,
				Hash:    hashFromDownload,
				Version: versions[0],
			}, nil
		}
	}

	// Fall back: try to update the existing source URL.
	// This will not work if releases are in different sub directories.
	u = c.SourceURL()
	u.Path = path.Dir(u.Path)
	u.Path = path.Join(u.Path, newBase)
	log.Printf("fallback: %s", u.String())
	return &CheckResult{
		Source:  u.String(),
		Hash:    hashFromDownload,
		Version: versions[0],
	}, nil
}

// e.g. checkGoMod(github.com/lpar/gzipped@v1.1.0)
func checkGoMod(v string) (*CheckResult, error) {
	mod := v
	if idx := strings.Index(mod, "@"); idx > -1 {
		mod = mod[:idx]
	}
	// https://github.com/golang/go/blob/master/src/cmd/go/internal/modfetch/proxy.go
	modEsc, err := module.EscapePath(mod)
	if err != nil {
		return nil, err
	}
	u := "https://proxy.golang.org/" + modEsc + "/@latest"
	resp, err := http.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: unexpected HTTP status: got %v, want OK", u, resp.Status)
	}
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var version struct{ Version string }
	if err := json.Unmarshal(b, &version); err != nil {
		return nil, err
	}

	remoteVersion := version.Version
	remoteSource := "distri+gomod://" + mod + "@" + remoteVersion
	return &CheckResult{
		Source:  remoteSource,
		Hash:    hashFromDownload,
		Version: remoteVersion,
	}, nil
}

func (c *check) checkSourceForge() (*CheckResult, error) {
	u := "https://sourceforge.net/projects/" + c.sourceForgeProject + "/best_release.json"
	resp, err := http.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: unexpected HTTP status: got %v, want OK", u, resp.Status)
	}
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var reply struct {
		PlatformReleases struct {
			Linux struct {
				URL      string `json:"url"`
				Filename string `json:"filename"`
			} `json:"linux"`
		} `json:"platform_releases"`
	}
	if err := json.Unmarshal(b, &reply); err != nil {
		return nil, err
	}

	versions, err := c.extractVersions(string(b))
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("no version found")
	}

	stripped, err := url.Parse(reply.PlatformReleases.Linux.URL)
	if err != nil {
		return nil, err
	}
	stripped.RawQuery = ""

	return &CheckResult{
		Source:  stripped.String(),
		Hash:    hashFromDownload,
		Version: versions[0],
	}, nil
}

var projectRe = regexp.MustCompile(`^/project/([^/]+)/`)

func Check(build []*ast.Node) (*CheckResult, error) {
	errNotSpecified := errors.New("not specified")
	valVal := func(path ...string) (string, error) {
		nodes := ast.GetFromPath(build, path)
		if len(nodes) == 0 {
			return "", errNotSpecified
		}
		if got, want := len(nodes), 1; got != want {
			return "", fmt.Errorf("malformed build file: got %d version keys, want %d", got, want)
		}
		values := nodes[0].Values
		if got, want := len(values), 1; got != want {
			return "", fmt.Errorf("malformed build file: got %d Values, want %d", got, want)
		}
		return values[0].Value, nil
	}
	stringVal := func(path ...string) (string, error) {
		v, err := valVal(path...)
		if err != nil {
			return "", err
		}
		return strconv.Unquote(v)
	}
	boolVal := func(path ...string) (bool, error) {
		s, err := valVal(path...)
		if err != nil {
			return false, err
		}
		b, err := strconv.ParseBool(s)
		if err != nil {
			return false, err
		}
		return b, nil
	}
	source, err := stringVal("source")
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(source)
	if err != nil {
		return nil, err
	}

	c := &check{
		source:    source,
		sourceURL: u,
	}

	if u.Host == "downloads.sourceforge.net" && strings.HasPrefix(u.Path, "/project/") {
		project := projectRe.FindStringSubmatch(u.Path)
		if got, want := len(project), 2; got != want {
			return nil, fmt.Errorf("downloads.sourceforge.net: got %d matches, want %d", got, want)
		}
		c.sourceForgeProject = project[1]
	}

	if strings.HasSuffix(source, ".deb") {
		u, err := stringVal("pull", "debian_packages")
		if err != nil {
			return nil, err
		}
		return c.checkDebian(u)
	}

	if strings.HasPrefix(source, "distri+gomod://") {
		return checkGoMod(strings.TrimPrefix(source, "distri+gomod://"))
	}

	version, err := stringVal("version")
	if err != nil {
		return nil, err
	}
	c.pv = distri.ParseVersion(version)

	// fall back: see if we can obtain an index page
	releases, err := stringVal("pull", "releases_url")
	releasesSpecified := err == nil
	if err != nil {
		u := c.SourceURL()
		if u.Host == "github.com" && strings.Contains(u.Path, "/releases/") {
			u.Path = u.Path[:strings.Index(u.Path, "/releases/")+len("/releases/")]
		} else if u.Host == "github.com" && strings.Contains(u.Path, "/archive/") {
			u.Path = u.Path[:strings.Index(u.Path, "/archive/")] + "/releases/"
		} else if u.Host == "downloads.sourceforge.net" && strings.HasPrefix(u.Path, "/project/") {
			u.Path = "/projects/" + c.sourceForgeProject + "/files/"
			u.Host = "sourceforge.net"
			// u e.g. https://sourceforge.net/projects/bzip2/files/
		} else if u.Host == "launchpad.net" {
			// e.g. https://launchpad.net/lightdm-gtk-greeter/2.0/2.0.6/+download
			u.Path = strings.TrimPrefix(u.Path, "/")
			if idx := strings.Index(u.Path, "/"); idx > -1 {
				u.Path = u.Path[:idx]
			}
			// e.g. https://launchpad.net/lightdm-gtk-greeter/
		} else {
			u.Path = path.Dir(u.Path) + "/"
		}
		releases = u.String()
	}

	log.Printf("releases: %s", releases)

	c.rePatternExpr, err = stringVal("pull", "release_regexp")
	if err != nil && err != errNotSpecified {
		return nil, fmt.Errorf("pull.release_regexp: %v", err)
	}

	c.replaceAllExpr, err = stringVal("pull", "release_replace_all", "expr")
	if err != nil && err != errNotSpecified {
		return nil, fmt.Errorf("pull.release_replace_all.expr: %v", err)
	}

	c.replaceAllRepl, err = stringVal("pull", "release_replace_all", "repl")
	if err != nil && err != errNotSpecified {
		return nil, fmt.Errorf("pull.release_replace_all.repl: %v", err)
	}

	c.forceSemver, err = boolVal("pull", "force_semver")
	if err != nil && err != errNotSpecified {
		return nil, fmt.Errorf("pull.force_semver: %v", err)
	}

	if u := c.SourceURL(); !releasesSpecified && (u.Host == "sourceforge.net" || u.Host == "downloads.sourceforge.net") {
		// No explicit releases_url was specified, so default to using the
		// SourceForge API:
		remote, err := c.checkSourceForge()
		if err == nil {
			return remote, nil
		}
		log.Println(err)
	}

	return c.checkHeuristic(releases)
}
