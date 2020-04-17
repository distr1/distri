package distri

type Repo struct {
	// Path is a file system path (e.g. /home/michael/distri/build/distri) or
	// HTTP URL (e.g. http://repo.distr1.org/).
	Path string

	// PkgPath is Path/pkg (e.g. /home/michael/distri/build/distri/pkg).
	PkgPath string
}
