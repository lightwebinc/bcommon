package record_test

import (
	"fmt"

	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/record"
)

// The sample record's magic and bounds: the test prefix, a letter and a
// version byte. An application registers its own.
var (
	sampleMagic = []byte("vxr\x01")
	sampleLast  = uint64(2) // the last key this version defines
	sampleMax   = 256       // the record's bound, in bytes
)

// An application's decoder reads its own keys, in its own order, from a
// record the package has held to the steps every record shares. A key above
// the last defined one is preserved, so a reader of this version writes
// back what a later version added.
func ExampleDecode() {
	b, err := record.Encode(cbor.Map{
		{Key: uint64(0), Val: sampleMagic},
		{Key: uint64(1), Val: "a name"},
		{Key: uint64(2), Val: uint64(7)},
	}, cbor.Map{{Key: uint64(9), Val: "from a later version"}}, sampleMax)
	if err != nil {
		fmt.Println(err)
		return
	}

	f, err := record.Decode(b, sampleMax, sampleLast, sampleMagic)
	if err != nil {
		fmt.Println(err)
		return
	}
	name, err := f.Text(1)
	fmt.Println("name:", name, err)
	count, err := f.Uint(2, 1, 100)
	fmt.Println("count:", count, err)
	fmt.Println("preserved:", len(f.Extra), "pair, key", f.Extra[0].Key)

	again, err := record.Encode(cbor.Map{
		{Key: uint64(0), Val: sampleMagic},
		{Key: uint64(1), Val: name},
		{Key: uint64(2), Val: count},
	}, f.Extra, sampleMax)
	fmt.Println("re-encodes to the same bytes:", string(again) == string(b), err)
	fmt.Println("claims the record:", record.Claims(b, sampleMagic), record.Claims(b, []byte("vxq\x01")))
	// Output:
	// name: a name <nil>
	// count: 7 <nil>
	// preserved: 1 pair, key 9
	// re-encodes to the same bytes: true <nil>
	// claims the record: true false
}

// A record is refused for the first rule it breaks, and each refusal has
// the fixed label a host counts it by.
func ExampleReason() {
	enc := func(m cbor.Map) []byte {
		b, err := cbor.Encode(m)
		if err != nil {
			panic(err)
		}
		return b
	}
	array, err := cbor.Encode([]cbor.Value{sampleMagic, "a name"})
	if err != nil {
		panic(err)
	}
	good := cbor.Map{{Key: uint64(0), Val: sampleMagic}, {Key: uint64(1), Val: "a name"}, {Key: uint64(2), Val: uint64(7)}}
	read := func(b []byte) error {
		f, err := record.Decode(b, sampleMax, sampleLast, sampleMagic)
		if err != nil {
			return err
		}
		if _, err := f.Text(1); err != nil {
			return err
		}
		_, err = f.Uint(2, 1, 100)
		return err
	}
	for _, c := range []struct {
		name string
		b    []byte
	}{
		{"over the bound, whatever it is", make([]byte, sampleMax+1)},
		{"not CBOR", []byte{0xff}},
		{"an array", array},
		{"a text key", enc(append(cbor.Map{{Key: "name", Val: uint64(1)}}, good...))},
		{"another magic", enc(cbor.Map{{Key: uint64(0), Val: []byte("vxq\x01")}})},
		{"key 1 missing", enc(cbor.Map{good[0], good[2]})},
		{"key 1 a number", enc(cbor.Map{good[0], {Key: uint64(1), Val: uint64(1)}, good[2]})},
		{"key 2 out of range", enc(cbor.Map{good[0], good[1], {Key: uint64(2), Val: uint64(101)}})},
	} {
		reason, _ := record.Reason(read(c.b))
		fmt.Printf("%s: %s\n", c.name, reason)
	}
	// Output:
	// over the bound, whatever it is: too-large
	// not CBOR: cbor
	// an array: cbor
	// a text key: key-type
	// another magic: magic
	// key 1 missing: missing
	// key 1 a number: type
	// key 2 out of range: range
}
