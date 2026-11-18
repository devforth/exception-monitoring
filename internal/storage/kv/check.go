package kv

import (
	"bytes"
	"errors"
	"fmt"
)

// Check verifies a collection against its declaration: every record has
// all its index keys, pointing to it, and every index key points to a
// record that has it. It reads everything, for tests and diagnostics.
func (collection *Collection[T]) Check(reader Reader) ([]string, error) {
	var problems []string

	err := collection.Primary().Each(reader, func(entry Entry[T]) (bool, error) {
		primary := collection.primaryKey(entry.Key)

		if !bytes.Equal(collection.KeyOf(entry.Record), entry.Key) {
			problems = append(problems, fmt.Sprintf("%s %x: stored under another key than its own", collection.name, primary))
		}

		for _, key := range collection.indexKeys(entry.Record) {
			holder, err := reader.Get(key.key)

			switch {
			case errors.Is(err, ErrNotFound):
				problems = append(problems, fmt.Sprintf("%s %x: index %s key %x missing", collection.name, primary, key.index, key.key))
			case err != nil:
				return false, err
			case !bytes.Equal(holder, primary):
				problems = append(problems, fmt.Sprintf("%s %x: index %s key %x points to %x", collection.name, primary, key.index, key.key, holder))
			}
		}

		return true, nil
	})

	if err != nil {
		return problems, err
	}

	for _, entry := range collection.order {
		iterator := reader.Iterate(entry.prefix, prefixEnd(entry.prefix))

		for valid := iterator.First(); valid; valid = iterator.Next() {
			key, primary := bytes.Clone(iterator.Key()), bytes.Clone(iterator.Value())
			value, err := reader.Get(primary)

			if errors.Is(err, ErrNotFound) {
				problems = append(problems, fmt.Sprintf("%s.%s key %x points to missing %x", collection.name, entry.Name, key, primary))

				continue
			}

			if err != nil {
				_ = iterator.Close()

				return problems, err
			}

			record, err := collection.definition.Decode(value)

			if err != nil {
				_ = iterator.Close()

				return problems, err
			}

			if own := entry.Key(record); own == nil || !bytes.Equal(append(bytes.Clone(entry.prefix), own...), key) {
				problems = append(problems, fmt.Sprintf("%s.%s key %x is not the key of the record it points to", collection.name, entry.Name, key))
			}
		}

		if err := iterator.Close(); err != nil {
			return problems, err
		}
	}

	return problems, nil
}
