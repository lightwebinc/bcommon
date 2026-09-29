package cbor

import (
	"bytes"
	"errors"
	"testing"
)

// The depth cases in cbor_test.go build their inputs from MaxDepth, so they
// pass whatever the bound is. The bound is part of the format: an item one
// reader accepts and another refuses splits them, and a reader in another
// language keeps its own copy of the number. These bytes do not move with the
// constant.
func TestDepthBoundIsSixteen(t *testing.T) {
	nested := func(n int) []byte {
		return append(bytes.Repeat([]byte{0x81}, n), 0x00)
	}
	if _, err := DecodeValue(nested(16)); err != nil {
		t.Errorf("sixteen nested arrays were refused: %v", err)
	}
	if _, err := DecodeValue(nested(17)); !errors.Is(err, ErrDepth) {
		t.Errorf("seventeen nested arrays: got %v, want ErrDepth", err)
	}
	var v Value = uint64(0)
	for i := 0; i < 16; i++ {
		v = []Value{v}
	}
	if b, err := Encode(v); err != nil || !bytes.Equal(b, nested(16)) {
		t.Errorf("encode sixteen deep: %x, %v", b, err)
	}
	if _, err := Encode([]Value{v}); !errors.Is(err, ErrDepth) {
		t.Errorf("encode seventeen deep: got %v, want ErrDepth", err)
	}
}
