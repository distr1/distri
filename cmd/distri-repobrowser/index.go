package main

import (
	"html/template"

	"github.com/distr1/distri"
)

var indexTmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"parseVersion": distri.ParseVersion,
}).Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">

  <meta name="viewport" content="width=device-width, initial-scale=1, shrink-to-fit=no">
  <meta name="google" content="notranslate">

  <title>distri repo browser: {{ .Repo }}</title>

  <link rel="stylesheet" href="https://stackpath.bootstrapcdn.com/bootstrap/4.4.1/css/bootstrap.min.css" crossorigin="anonymous">
  <style type="text/css">
td.upstreamversion {
  max-width: 400px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

td.upstreamversion badge {
  display: inline;
}
</style>
</head>
<body>
<div class="container">
<h1>distri repo browser</h1>
<p>
  Repository: <code>{{ .Repo }}</code>
</p>
<table width="100%" class="table table-striped table-hover table-sm">
<thead>
  <tr>
    <th>Package</th>
    <th>Architecture</th>
    <th>Upstream Version</th>
    <th>distri Revision</th>
    <th>Links</th>
  </tr>
</thead>
{{ range $idx, $pkg := .Packages }}
  {{ with $pv := parseVersion $pkg.Name }}
  <tr>
    <td>
{{ $pv.Pkg }}

{{ with $status := index $.UpstreamStatus $pv.Pkg }}
{{ if (and (ne $pv.Upstream "native") (or (eq $status.SourcePackage "") $status.Unreachable)) }}
<span class="badge badge-danger">unreachable</span>
{{ end }}
{{ end }}

<!-- TODO: flag unreachable packages -->
    </td>

    <td>{{ $pv.Arch }}</td>

    <td class="upstreamversion">
{{ $pv.Upstream }}

{{ with $status := index $.UpstreamStatus $pv.Pkg }}
{{ if (and (ne $status.SourcePackage "") (not $status.Unreachable) (ne $status.UpstreamVersion $pv.Upstream)) }}
<span class="badge badge-warning">upstream: {{ $status.UpstreamVersion }}</span>
{{ end }}
{{ if (and (ne $status.SourcePackage "") (not $status.Unreachable) (eq $status.UpstreamVersion $pv.Upstream)) }}
<span class="badge badge-success">up-to-date</span>
{{ end }}
{{ end }}
    </td>

    <td>{{ $pv.DistriRevision }}</td>

    <td>
<!-- TODO: use correct branch from repo url -->
{{ with $branch := "master" }}
<a href="https://github.com/distr1/distri/blob/{{ $branch }}/pkgs/{{ $pv.Pkg }}/build.textproto">build file</a>
{{ end }}
</td>
  </tr>
  {{ end }}
{{ end }}
</table>
</div>
<script src="https://code.jquery.com/jquery-3.4.1.slim.min.js" integrity="sha384-J6qa4849blE2+poT4WnyKhv5vZF5SrPo0iEjwBvKU7imGFAV0wwj1yYfoRSJoZ+n" crossorigin="anonymous"></script>
<script src="https://cdn.jsdelivr.net/npm/popper.js@1.16.0/dist/umd/popper.min.js" integrity="sha384-Q6E9RHvbIyZFJoft+2mJbHaEWldlvI9IOYy5n3zV9zzTtmI3UksdQRVvoxMfooAo" crossorigin="anonymous"></script>
<script src="https://stackpath.bootstrapcdn.com/bootstrap/4.4.1/js/bootstrap.min.js" integrity="sha384-wfSDF2E50Y2D1uUdj0O3uMBJnjuUD4Ih7YwaYd1iqfktj0Uod8GCExl3Og8ifwB6" crossorigin="anonymous"></script>
</body>
</html>
`))
