package kv

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"testing"
)

// item is a record of the test collection.
type item struct {
	ID      uint64
	Project uint32
	Level   byte
	Trace   string
	Name    string
}

type testStore struct {
	db    *DB
	items *Collection[item]
}

func defineItems(schema *Schema) *Collection[item] {
	return Define(schema, Definition[item]{
		Name: "item",
		Key:  func(record item) Key { return K(U64(record.ID)) },
		Indexes: []Index[item]{
			{Name: "project", Key: func(record item) Key { return K(U32(record.Project), U8(record.Level), U64(record.ID)) }},
			{Name: "trace", Key: func(record item) Key { return KIf(record.Trace != "", Text(record.Trace), U64(record.ID)) }},
			{Name: "name", Key: func(record item) Key { return KIf(record.Name != "", Text(record.Name)) }, Unique: true},
		},
		Counter: func(record item) Key { return K(Text("item"), U8(record.Level)) },
	})
}

func openStore(t *testing.T, engine Engine) testStore {
	t.Helper()

	schema := NewSchema()
	items := defineItems(schema)
	db, err := OpenDB(engine, schema)

	if err != nil {
		t.Fatal(err)
	}

	return testStore{db: db, items: items}
}

func newStore(t *testing.T) testStore {
	t.Helper()

	store := openStore(t, OpenMemory())
	t.Cleanup(func() { _ = store.db.Close() })

	return store
}

func (store testStore) insert(t *testing.T, records ...item) {
	t.Helper()

	err := store.db.Update(false, func(batch *Batch) error {
		for _, record := range records {
			if err := store.items.Insert(batch, record); err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		t.Fatal(err)
	}
}

func (store testStore) check(t *testing.T) {
	t.Helper()

	if problems, err := store.items.Check(store.db); err != nil || len(problems) > 0 {
		t.Fatalf("check: %v %v", problems, err)
	}
}

func ids(records []item) []uint64 {
	var result []uint64

	for _, record := range records {
		result = append(result, record.ID)
	}

	return result
}

// Parts sort the way their values do, a text never matches a longer one,
// and keys decode back to their parts.
func TestKeys(t *testing.T) {
	ordered := []Key{
		K(Text("a")), K(Text("a\x00")), K(Text("a\x00b")), K(Text("ab")), K(Text("b")),
	}

	for index := 1; index < len(ordered); index++ {
		if bytes.Compare(ordered[index-1], ordered[index]) >= 0 {
			t.Errorf("%q sorts after %q", ordered[index-1], ordered[index])
		}
	}

	if bytes.HasPrefix(K(Text("ab")), K(Text("a"))) {
		t.Error("a text key is a prefix of a longer text")
	}

	if bytes.Compare(K(U32(255)), K(U32(256))) >= 0 || bytes.Compare(K(U64(1<<40)), K(U64(1<<41))) >= 0 {
		t.Error("numbers do not sort by value")
	}

	key := K(U32(7), Text("x\x00y"), U8(3), U64(1<<60))
	decoder := Decode(key, 0)

	if decoder.U32() != 7 || decoder.Text() != "x\x00y" || decoder.U8() != 3 || decoder.U64() != 1<<60 || decoder.Err() != nil {
		t.Fatalf("decode %x: %v", key, decoder.Err())
	}

	if decoder := Decode(K(U8(1)), 0); decoder.U64() != 0 || !errors.Is(decoder.Err(), ErrBadKey) {
		t.Error("a short key decodes")
	}
}

// A prefix that is another one's prefix would mix their keys: registering
// it stops the program.
func TestSchemaRefusesOverlaps(t *testing.T) {
	refused := func(name string, register func(schema *Schema)) {
		t.Helper()

		defer func() {
			if recover() == nil {
				t.Errorf("%s: no panic", name)
			}
		}()

		schema := NewSchema()
		defineItems(schema)
		register(schema)
	}

	refused("same collection", func(schema *Schema) { defineItems(schema) })
	refused("prefix of an index", func(schema *Schema) { schema.Reserve("item.project") })
	refused("separator in a name", func(schema *Schema) { schema.Reserve("a/b") })

	schema := NewSchema()
	defineItems(schema)
	schema.Reserve("items") // "items/" and "item/" do not overlap

	if prefixes := schema.Prefixes(); !slices.Equal(prefixes, []string{"item.name/", "item.project/", "item.trace/", "item/", "items/"}) {
		t.Errorf("prefixes %v", prefixes)
	}
}

// Records are found through every index, newest first, within bounds, and
// a filter checks the records the walk reads.
func TestQueries(t *testing.T) {
	store := newStore(t)

	for id := uint64(1); id <= 9; id++ {
		store.insert(t, item{ID: id, Project: uint32(1 + id%2), Level: byte(id % 3), Trace: fmt.Sprintf("trace-%d", id%3), Name: fmt.Sprintf("name-%d", id)})
	}

	store.check(t)

	for name, test := range map[string]struct {
		query Query[item]
		want  []uint64
	}{
		"primary":                  {store.items.Primary(), []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9}},
		"primary newest, 3":        {store.items.Primary().Newest().Limit(3), []uint64{9, 8, 7}},
		"primary from 4 before 7":  {store.items.Primary().From(U64(4)).Before(U64(7)), []uint64{4, 5, 6}},
		"project 2, level 1":       {store.items.By("project", U32(2), U8(1)), []uint64{1, 7}},
		"project 1 newest":         {store.items.By("project", U32(1)).Newest(), []uint64{8, 2, 4, 6}}, // by level, then ID
		"trace, newest, before 7":  {store.items.By("trace", Text("trace-1")).Newest().Before(U64(7)), []uint64{4, 1}},
		"where odd, newest, 2":     {store.items.Primary().Newest().Where(func(record item) bool { return record.ID%2 == 1 }).Limit(2), []uint64{9, 7}},
		"prefix of nothing stored": {store.items.By("project", U32(9)), nil},
	} {
		records, err := test.query.All(store.db)

		if err != nil || !slices.Equal(ids(records), test.want) {
			t.Errorf("%s: %v (%v), want %v", name, ids(records), err, test.want)
		}
	}

	if count, more, err := store.items.By("project", U32(1)).Count(store.db, 3); count != 3 || !more || err != nil {
		t.Errorf("count up to 3: %d %v %v", count, more, err)
	}

	if found, err := store.items.GetBy(store.db, "name", K(Text("name-5"))); err != nil || found.ID != 5 {
		t.Errorf("by unique name: %+v %v", found, err)
	}

	if _, err := store.items.Get(store.db, K(U64(99))); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing record: %v", err)
	}
}

// Saving a changed record moves its index keys: the old ones are gone, and
// the counter follows the record from one level to the other.
func TestSaveMovesIndexKeys(t *testing.T) {
	store := newStore(t)
	store.insert(t, item{ID: 1, Project: 1, Level: 1, Trace: "old", Name: "first"})

	err := store.db.Update(false, func(batch *Batch) error {
		return store.items.Save(batch, item{ID: 1, Project: 2, Level: 2, Name: "renamed"})
	})

	if err != nil {
		t.Fatal(err)
	}

	store.check(t)

	if records, _ := store.items.By("project", U32(1)).All(store.db); len(records) != 0 {
		t.Errorf("old project still lists it: %v", records)
	}

	if records, _ := store.items.By("trace").All(store.db); len(records) != 0 {
		t.Errorf("a trace is left: %v", records)
	}

	if _, err := store.items.GetBy(store.db, "name", K(Text("first"))); !errors.Is(err, ErrNotFound) {
		t.Errorf("old name still taken: %v", err)
	}

	if store.db.Counter(K(Text("item"), U8(1))) != 0 || store.db.Counter(K(Text("item"), U8(2))) <= 0 {
		t.Errorf("counters %v", store.db.Counters())
	}
}

// A unique key held by another record is refused, also within one batch;
// the same record may save it again.
func TestUniqueIndex(t *testing.T) {
	store := newStore(t)
	store.insert(t, item{ID: 1, Name: "taken"})

	var conflict ErrConflict

	err := store.db.Update(false, func(batch *Batch) error { return store.items.Insert(batch, item{ID: 2, Name: "taken"}) })

	if !errors.As(err, &conflict) || conflict.Index != "name" {
		t.Errorf("a taken name: %v", err)
	}

	err = store.db.Update(false, func(batch *Batch) error {
		if err := store.items.Insert(batch, item{ID: 3, Name: "new"}); err != nil {
			return err
		}

		return store.items.Insert(batch, item{ID: 4, Name: "new"})
	})

	if !errors.As(err, &conflict) {
		t.Errorf("a name taken earlier in the batch: %v", err)
	}

	if _, err := store.items.Get(store.db, K(U64(3))); !errors.Is(err, ErrNotFound) {
		t.Error("a failed update wrote part of its batch")
	}

	if err := store.db.Update(false, func(batch *Batch) error { return store.items.Save(batch, item{ID: 1, Name: "taken", Level: 2}) }); err != nil {
		t.Errorf("the holder saving again: %v", err)
	}

	store.check(t)
}

// Deleting removes the index keys and takes the bytes back from the
// counter, whether the record comes from a walk or by key.
func TestDelete(t *testing.T) {
	store := newStore(t)

	for id := uint64(1); id <= 4; id++ {
		store.insert(t, item{ID: id, Project: 1, Level: 3, Trace: "t", Name: fmt.Sprintf("n%d", id)})
	}

	err := store.db.Update(false, func(batch *Batch) error {
		if _, err := store.items.DeleteKey(batch, K(U64(1))); err != nil {
			return err
		}

		if err := store.items.Delete(batch, item{ID: 2, Project: 1, Level: 3, Trace: "t", Name: "n2"}); err != nil {
			return err
		}

		return store.items.By("project", U32(1)).From(U8(3), U64(3)).Each(store.db, func(entry Entry[item]) (bool, error) {
			store.items.DeleteEntry(batch, entry)

			return true, nil
		})
	})

	if err != nil {
		t.Fatal(err)
	}

	iterator := store.db.Iterate(nil, nil)

	for valid := iterator.First(); valid; valid = iterator.Next() {
		if !bytes.HasPrefix(iterator.Key(), []byte("meta/")) {
			t.Errorf("left after deleting everything: %q", iterator.Key())
		}
	}

	_ = iterator.Close()

	if counters := store.db.Counters(); len(counters) != 0 {
		t.Errorf("counters %v", counters)
	}
}

// Check finds an index key that is missing and one that points nowhere.
func TestCheckFindsProblems(t *testing.T) {
	store := newStore(t)
	store.insert(t, item{ID: 1, Project: 1, Trace: "t"}, item{ID: 2, Project: 1})

	batch := NewBatch(store.db)
	batch.Delete(store.items.indexes["trace"].prefix.With(Text("t"), U64(1)))
	batch.Delete(store.items.primaryKey(K(U64(2))))

	if err := store.db.Write(batch, false); err != nil {
		t.Fatal(err)
	}

	problems, err := store.items.Check(store.db)

	if err != nil || len(problems) != 2 {
		t.Fatalf("problems %v %v", problems, err)
	}
}

// Sequence numbers keep growing across a restart, also after a crash
// (numbers handed out but never stored are skipped), and counters survive.
func TestRestart(t *testing.T) {
	dir := t.TempDir()
	engine, err := Open(dir)

	if err != nil {
		t.Fatal(err)
	}

	store := openStore(t, engine)
	store.insert(t, item{ID: 1, Level: 1})

	var last uint64

	for range 3 {
		if last, err = store.db.Next(); err != nil {
			t.Fatal(err)
		}
	}

	if size, err := store.db.Size(); err != nil || size == 0 {
		t.Errorf("size %d %v", size, err)
	}

	counted := store.db.Counter(K(Text("item"), U8(1)))
	_ = store.db.Close()

	if engine, err = Open(dir); err != nil {
		t.Fatal(err)
	}

	store = openStore(t, engine)
	t.Cleanup(func() { _ = store.db.Close() })

	if next, err := store.db.Next(); err != nil || next <= last {
		t.Errorf("after a restart: %d, before %d (%v)", next, last, err)
	}

	if counter := store.db.Counter(K(Text("item"), U8(1))); counter != counted || counted == 0 {
		t.Errorf("counter %d, before %d", counter, counted)
	}
}
