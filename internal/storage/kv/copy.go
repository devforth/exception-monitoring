package kv

import (
	"bytes"
	"context"
)

// copyBatchBytes bounds one batch of Copy.
const copyBatchBytes = 4 << 20

// Copy writes every key of from into to in batches; progress gets the totals after each batch.
func Copy(ctx context.Context, from Reader, to Engine, progress func(keys, bytes int64)) (keys, total int64, err error) {
	iterator := from.Iterate(nil, nil)

	defer func() {
		if closeErr := iterator.Close(); err == nil {
			err = closeErr
		}
	}()

	batch := NewBatch(to)
	batchBytes := 0

	flush := func() error {
		if err := to.Write(batch, false); err != nil {
			return err
		}

		if progress != nil {
			progress(keys, total)
		}

		batch = NewBatch(to)
		batchBytes = 0

		return ctx.Err()
	}

	for valid := iterator.First(); valid; valid = iterator.Next() {
		// The iterator reuses its slices.
		batch.Put(bytes.Clone(iterator.Key()), bytes.Clone(iterator.Value()))

		size := len(iterator.Key()) + len(iterator.Value())
		keys++
		total += int64(size)
		batchBytes += size

		if batchBytes >= copyBatchBytes {
			if err := flush(); err != nil {
				return keys, total, err
			}
		}
	}

	if err := flush(); err != nil {
		return keys, total, err
	}

	return keys, total, nil
}
