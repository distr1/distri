package repo

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/distr1/distri"
)

type ErrNotFound struct {
	url *url.URL
}

func (e ErrNotFound) Error() string {
	return fmt.Sprintf("%v: HTTP status 404", e.url)
}

type gzipReader struct {
	body io.ReadCloser
	zr   *gzip.Reader
}

func (r *gzipReader) Read(p []byte) (n int, err error) {
	return r.zr.Read(p)
}

func (r *gzipReader) Close() error {
	if err := r.zr.Close(); err != nil {
		return err
	}
	return r.body.Close()
}

var httpClient = &http.Client{Transport: &http.Transport{
	MaxIdleConnsPerHost: 10,
	DisableCompression:  true,
}}

func Reader(ctx context.Context, repo distri.Repo, fn string) (io.ReadCloser, error) {
	if strings.HasPrefix(repo.Path, "http://") ||
		strings.HasPrefix(repo.Path, "https://") {
		req, err := http.NewRequest("GET", repo.Path+"/"+fn, nil) // TODO: sanitize slashes
		if err != nil {
			return nil, err
		}
		if os.Getenv("DISTRI_REEXEC") == "1" {
			req.Header.Set("X-Distri-Reexec", "yes")
		}
		// good for typical links (≤ gigabit)
		// performance bottleneck for faster links (10 gbit/s+)
		req.Header.Set("Accept-Encoding", "gzip")
		resp, err := httpClient.Do(req.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		if got, want := resp.StatusCode, http.StatusOK; got != want {
			if got == http.StatusNotFound {
				return nil, &ErrNotFound{url: req.URL}
			}
			return nil, fmt.Errorf("%s: HTTP status %v", req.URL, resp.Status)
		}
		if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
			rd, err := gzip.NewReader(resp.Body)
			if err != nil {
				return nil, err
			}
			return &gzipReader{body: resp.Body, zr: rd}, nil
		}
		return resp.Body, nil
	}
	return os.Open(filepath.Join(repo.Path, fn))
}
