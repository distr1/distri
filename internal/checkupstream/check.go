package checkupstream

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/protocolbuffers/txtpbfmt/ast"
)

func checkDebian(debianPackagesURL, source string) (remoteSource, remoteHash, remoteVersion string, _ error) {
	ctx, canc := context.WithTimeout(context.Background(), 5*time.Second)
	defer canc()
	req, err := http.NewRequestWithContext(ctx, "GET", debianPackagesURL, nil)
	if err != nil {
		return "", "", "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		return "", "", "", fmt.Errorf("%v: HTTP %v", debianPackagesURL, resp.Status)
	}
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", err
	}
	lines := strings.Split(string(b), "\n")
	sourceUrl, err := url.Parse(source)
	if err != nil {
		return "", "", "", err
	}
	sourceUrl.Path = path.Dir(sourceUrl.Path)
	var filename string
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
		return sourceUrl.String(), remoteHash, remoteVersion, nil
	}
	return "", "", "", fmt.Errorf("package not found in Debian Packages file")
}

func Check(build []*ast.Node) (source, hash, version string, _ error) {
	stringVal := func(path ...string) (string, error) {
		nodes := ast.GetFromPath(build, path)
		if got, want := len(nodes), 1; got != want {
			return "", fmt.Errorf("malformed build file: got %d version keys, want %d", got, want)
		}
		values := nodes[0].Values
		if got, want := len(values), 1; got != want {
			return "", fmt.Errorf("malformed build file: got %d Values, want %d", got, want)
		}
		return strconv.Unquote(values[0].Value)
	}
	source, err := stringVal("source")
	if err != nil {
		return "", "", "", err
	}

	if strings.HasSuffix(source, ".deb") {
		u, err := stringVal("pull", "debian_packages")
		if err != nil {
			return "", "", "", err
		}
		return checkDebian(u, source)
	}
	return "", "", "", fmt.Errorf("not yet implemented")
}
