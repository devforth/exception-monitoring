package kv

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The libraries of stores an adapter may wrap, and the one file that may
// import each. Anything else importing one would tie the code to that store
// and pull it into every build.
var adapters = map[string]string{
	"github.com/syndtr/goleveldb": "internal/storage/kv/goleveldb.go",
}

// Store libraries the project does not use at all; none may come back.
var unusedStores = []string{
	"github.com/cockroachdb/pebble", "github.com/dgraph-io/badger", "go.etcd.io/bbolt", "github.com/jmhodges/levigo",
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
				if strings.HasPrefix(importPath, library) && filepath.ToSlash(relative) != adapter {
					t.Errorf("%s imports %s; only %s may", relative, importPath, adapter)
				}
			}

			for _, library := range unusedStores {
				if strings.HasPrefix(importPath, library) {
					t.Errorf("%s imports %s, a store the project does not use", relative, importPath)
				}
			}
		}

		return nil
	})

	if err != nil {
		t.Fatal(err)
	}

	module, err := os.ReadFile(filepath.Join(root, "go.mod"))

	if err != nil {
		t.Fatal(err)
	}

	for _, library := range unusedStores {
		if strings.Contains(string(module), library) {
			t.Errorf("go.mod requires %s", library)
		}
	}
}
