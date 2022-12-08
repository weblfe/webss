package assets

import (
	"embed"
	"io/fs"
)

//go:embed *.html
var fSys embed.FS

func GetFs() fs.FS {
	return fSys
}
