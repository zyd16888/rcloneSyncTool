package server

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
)

// Fingerprints follow the embedded bytes, including local builds without a tag.
var staticVersions = func() map[string]string {
	versions := make(map[string]string)
	files, err := fs.Glob(content, "static/*")
	if err != nil {
		panic(err)
	}
	for _, file := range files {
		data, err := content.ReadFile(file)
		if err != nil {
			panic(err)
		}
		sum := sha256.Sum256(data)
		versions["/"+file] = fmt.Sprintf("%x", sum[:8])
	}
	return versions
}()

func staticURL(name string) string {
	path := "/static/" + name
	return path + "?v=" + staticVersions[path]
}
