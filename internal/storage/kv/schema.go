package kv

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Schema registers the key prefixes of a database; registering one that
// overlaps another panics, so a conflict stops the program at start.
type Schema struct {
	mutex    sync.Mutex
	prefixes map[string]string // prefix -> what owns it
}

// NewSchema starts an empty schema.
func NewSchema() *Schema { return &Schema{prefixes: map[string]string{}} }

// Reserve registers a raw prefix, name + "/", and returns it.
func (schema *Schema) Reserve(name string) Key {
	prefix := name + "/"
	schema.register(prefix, "reserved "+name)

	return Key(prefix)
}

func (schema *Schema) register(prefix, owner string) {
	schema.mutex.Lock()
	defer schema.mutex.Unlock()

	if strings.ContainsAny(strings.TrimSuffix(prefix, "/"), "/\x00") {
		panic(fmt.Sprintf("kv: name of %s holds a separator: %q", owner, prefix))
	}

	for existing, other := range schema.prefixes {
		if strings.HasPrefix(existing, prefix) || strings.HasPrefix(prefix, existing) {
			panic(fmt.Sprintf("kv: prefix %q of %s overlaps %q of %s", prefix, owner, existing, other))
		}
	}

	schema.prefixes[prefix] = owner
}

// Prefixes lists the registered prefixes in order.
func (schema *Schema) Prefixes() []string {
	schema.mutex.Lock()
	defer schema.mutex.Unlock()

	prefixes := make([]string, 0, len(schema.prefixes))

	for prefix := range schema.prefixes {
		prefixes = append(prefixes, prefix)
	}

	sort.Strings(prefixes)

	return prefixes
}
