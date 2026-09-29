package cbor_test

import (
	"fmt"

	"github.com/lightwebinc/bcommon/cbor"
)

// Encode sorts a map's keys into canonical order (bytewise order of their
// encodings), whatever order the Map lists them in, and writes every head in
// its shortest form. Decoding gives the pairs back in wire order.
func ExampleEncode() {
	record := cbor.Map{
		{Key: "name", Val: "example"},
		{Key: uint64(2), Val: []byte{0xca, 0xfe}},
		{Key: uint64(1), Val: int64(-1)},
		{Key: "tags", Val: []cbor.Value{"a", true, nil}},
	}
	b, err := cbor.Encode(record)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("%x\n", b)

	v, err := cbor.DecodeValue(b)
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, p := range v.(cbor.Map) {
		fmt.Printf("%v => %v\n", p.Key, p.Val)
	}
	name, _ := v.(cbor.Map).Get("name")
	fmt.Println("name:", name)
	// Output:
	// a401200242cafe646e616d65676578616d706c656474616773836161f5f6
	// 1 => -1
	// 2 => [202 254]
	// name => example
	// tags => [a true <nil>]
	// name: example
}

// The decoder refuses every encoding the encoder would not have written, so
// a value has exactly one byte string a reader accepts.
func ExampleDecodeValue_refusals() {
	for _, c := range []struct {
		what string
		b    []byte
	}{
		{"23 in two bytes", []byte{0x18, 0x17}},
		{"map keys out of order", []byte{0xa2, 0x02, 0x00, 0x01, 0x00}},
		{"duplicate map key", []byte{0xa2, 0x01, 0x00, 0x01, 0x00}},
		{"indefinite-length array", []byte{0x9f, 0x01, 0xff}},
		{"a float", []byte{0xf9, 0x3c, 0x00}},
		{"trailing bytes", []byte{0x01, 0x02}},
		{"truncated string", []byte{0x43, 0x01}},
	} {
		_, err := cbor.DecodeValue(c.b)
		fmt.Printf("%s: %v\n", c.what, err)
	}
	// Output:
	// 23 in two bytes: cbor: not canonical
	// map keys out of order: cbor: map keys out of order
	// duplicate map key: cbor: duplicate map key
	// indefinite-length array: cbor: unsupported item
	// a float: cbor: unsupported item
	// trailing bytes: cbor: trailing bytes
	// truncated string: cbor: truncated
}
