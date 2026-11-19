// Package kv is the layer over an ordered key-value store: typed keys,
// collections with indexes, batches and index walks.
package kv

import (
	"bytes"
	"encoding/binary"
)

// Key is an encoded key or part of one; its parts sort the way their values do.
type Key []byte

// Part is one typed part of a key.
type Part struct {
	text   string
	number uint64
	width  byte // bytes of a number; 0 for text
}

// U8, U32 and U64 are number parts of 1, 4 and 8 bytes.
func U8(value byte) Part { return Part{number: uint64(value), width: 1} }

func U32(value uint32) Part { return Part{number: uint64(value), width: 4} }

func U64(value uint64) Part { return Part{number: value, width: 8} }

// Text is a text part; zero bytes are escaped, so a prefix never matches a longer text.
func Text(value string) Part { return Part{text: value} }

// K builds a key from parts.
func K(parts ...Part) Key { return Key(nil).With(parts...) }

// KIf builds a key from parts when condition holds, else returns nil.
func KIf(condition bool, parts ...Part) Key {
	if !condition {
		return nil
	}

	return K(parts...)
}

// With returns a new key: this one followed by parts.
func (key Key) With(parts ...Part) Key {
	result := make(Key, len(key), len(key)+len(parts)*9)
	copy(result, key)

	for _, part := range parts {
		result = part.appendTo(result)
	}

	return result
}

func (part Part) appendTo(key Key) Key {
	switch part.width {
	case 1:
		return append(key, byte(part.number))
	case 4:
		return binary.BigEndian.AppendUint32(key, uint32(part.number))
	case 8:
		return binary.BigEndian.AppendUint64(key, part.number)
	}

	for index := range len(part.text) {
		if part.text[index] == 0 {
			key = append(key, 0, escapedZero)
		} else {
			key = append(key, part.text[index])
		}
	}

	return append(key, 0, textEnd)
}

// A text part ends with 0x00 0x00; a zero byte inside it is 0x00 0xFF.
const (
	textEnd     = 0x00
	escapedZero = 0xFF
)

// Decoder reads the parts of a key in order; once a read fails, every later read returns zero.
type Decoder struct {
	rest   []byte
	failed bool
}

// Decode reads key from offset on.
func Decode(key []byte, offset int) *Decoder {
	if offset > len(key) {
		return &Decoder{failed: true}
	}

	return &Decoder{rest: key[offset:]}
}

func (decoder *Decoder) number(width int) uint64 {
	if decoder.failed || len(decoder.rest) < width {
		decoder.failed = true

		return 0
	}

	var value uint64

	for _, octet := range decoder.rest[:width] {
		value = value<<8 | uint64(octet)
	}

	decoder.rest = decoder.rest[width:]

	return value
}

func (decoder *Decoder) U8() byte { return byte(decoder.number(1)) }

func (decoder *Decoder) U64() uint64 { return decoder.number(8) }

func (decoder *Decoder) Text() string {
	if decoder.failed {
		return ""
	}

	var text []byte

	for index := 0; index+1 < len(decoder.rest); index++ {
		if decoder.rest[index] != 0 {
			text = append(text, decoder.rest[index])

			continue
		}

		switch decoder.rest[index+1] {
		case textEnd:
			decoder.rest = decoder.rest[index+2:]

			return string(text)
		case escapedZero:
			text = append(text, 0)
			index++
		default:
			decoder.failed = true

			return ""
		}
	}

	decoder.failed = true

	return ""
}

// prefixEnd returns the first key after every key with prefix, or nil if none.
func prefixEnd(prefix []byte) []byte {
	end := bytes.Clone(prefix)

	for index := len(end) - 1; index >= 0; index-- {
		if end[index] < 0xff {
			end[index]++

			return end[:index+1]
		}
	}

	return nil
}
