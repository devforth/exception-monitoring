package kv

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// The libraries of stores an adapter may wrap, and the one adapter package
// that may import each. Anything else importing one would tie the code to that store
// and pull it into every build.
var adapters = map[string]string{
	"github.com/syndtr/goleveldb": "internal/storage/kv/leveldb/",
}

func TestOnlyAdaptersImportStores(t *testing.T) {
	root := filepath.Join("..", "..", "..")

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			if name := entry.Name(); name == "testdata" || name == "node_modules" || (strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)

		if err != nil {
			return err
		}

		relative, _ := filepath.Rel(root, path)

		for _, imported := range file.Imports {
			importPath := strings.Trim(imported.Path.Value, `"`)

			for library, adapter := range adapters {
				if strings.HasPrefix(importPath, library) && !strings.HasPrefix(filepath.ToSlash(relative), adapter) {
					t.Errorf("%s imports %s; only %s may", relative, importPath, adapter)
				}
			}
		}

		return nil
	})

	if err != nil {
		t.Fatal(err)
	}
}
