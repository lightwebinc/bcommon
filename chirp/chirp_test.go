package chirp

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type vector struct {
	Golden []struct {
		Name       string `json:"name"`
		ContentHex string `json:"contentHex"`
		Extensions []struct {
			Type     uint64 `json:"type"`
			ValueHex string `json:"valueHex"`
		} `json:"extensions"`
		ContentHashHex string `json:"contentHashHex"`
		RootHex        string `json:"rootHex"`
		RootHashHex    string `json:"rootHashHex"`
		Identifier     string `json:"identifier"`
		URL            string `json:"url"`
	} `json:"golden"`
	Contents []struct {
		Name  string `json:"name"`
		Parts []struct {
			Stream int    `json:"stream"`
			Hex    string `json:"hex"`
		} `json:"parts"`
		Size           int      `json:"size"`
		ContentHashHex string   `json:"contentHashHex"`
		BlobHashesHex  []string `json:"blobHashesHex"`
		BranchesHex    []string `json:"branchesHex"`
		RootHex        string   `json:"rootHex"`
		RootHashHex    string   `json:"rootHashHex"`
		Identifier     string   `json:"identifier"`
		Objects        int      `json:"objects"`
	} `json:"contents"`
	Shapes []struct {
		Leaves         int    `json:"leaves"`
		Widths         []int  `json:"widths"`
		Branches       int    `json:"branches"`
		ContentHashHex string `json:"contentHashHex"`
		RootChildren   []struct {
			Kind    uint8  `json:"kind"`
			Length  uint64 `json:"length"`
			HashHex string `json:"hashHex"`
		} `json:"rootChildren"`
		RootHashHex string `json:"rootHashHex"`
	} `json:"shapes"`
	Nodes []struct {
		Name   string `json:"name"`
		As     string `json:"as"`
		Hex    string `json:"hex"`
		Reason string `json:"reason"`
	} `json:"nodes"`
	Closures []struct {
		Name        string `json:"name"`
		RootHashHex string `json:"rootHashHex"`
		Objects     []struct {
			HashHex string `json:"hashHex"`
			Hex     string `json:"hex"`
		} `json:"objects"`
		MaxReferences int    `json:"maxReferences"`
		MaxLength     uint64 `json:"maxLength"`
		Reason        string `json:"reason"`
		Canonical     bool   `json:"canonical"`
		Blobs         int    `json:"blobs"`
	} `json:"closures"`
	Identifiers []struct {
		Name    string `json:"name"`
		Input   string `json:"input"`
		URL     bool   `json:"url"`
		HashHex string `json:"hashHex"`
		Reason  string `json:"reason"`
	} `json:"identifiers"`
}

func load(t *testing.T) vector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "chirp-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v vector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func h32(t *testing.T, s string) [32]byte {
	t.Helper()
	b := unhex(t, s)
	if len(b) != 32 {
		t.Fatalf("hash %q", s)
	}
	return [32]byte(b)
}

func stream(n int) []byte {
	out := make([]byte, 0, n+32)
	for j := uint32(0); len(out) < n; j++ {
		h := sha256.Sum256(binary.BigEndian.AppendUint32([]byte("bcommon vector content"), j))
		out = append(out, h[:]...)
	}
	return out[:n]
}

func reason(err error) string {
	if err == nil {
		return ""
	}
	r, ok := Reason(err)
	if !ok {
		return "unnamed: " + err.Error()
	}
	return r
}

func TestGolden(t *testing.T) {
	for _, g := range load(t).Golden {
		var ext []Extension
		for _, e := range g.Extensions {
			ext = append(ext, Extension{Type: e.Type, Value: unhex(t, e.ValueHex)})
		}
		b, err := Build(unhex(t, g.ContentHex), ext)
		if err != nil {
			t.Fatalf("%s: %v", g.Name, err)
		}
		if hex.EncodeToString(b.Root) != g.RootHex || b.RootHash != h32(t, g.RootHashHex) {
			t.Errorf("%s: root %x", g.Name, b.Root)
		}
		if Identifier(b.RootHash) != g.Identifier || URL(b.RootHash) != g.URL {
			t.Errorf("%s: identifier %s", g.Name, Identifier(b.RootHash))
		}
		r, err := DecodeRoot(b.Root)
		if err != nil || r.ContentHash != h32(t, g.ContentHashHex) {
			t.Errorf("%s: decode %v", g.Name, err)
		}
		again, err := EncodeRoot(r)
		if err != nil || !bytes.Equal(again, b.Root) {
			t.Errorf("%s: re-encode differs", g.Name)
		}
	}
}

func TestContents(t *testing.T) {
	for _, c := range load(t).Contents {
		var content []byte
		for _, p := range c.Parts {
			if p.Hex != "" {
				content = append(content, unhex(t, p.Hex)...)
			} else {
				content = append(content, stream(p.Stream)...)
			}
		}
		if len(content) != c.Size {
			t.Fatalf("%s: %d bytes", c.Name, len(content))
		}
		b, err := Build(content, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if hex.EncodeToString(b.Root) != c.RootHex || b.RootHash != h32(t, c.RootHashHex) || Identifier(b.RootHash) != c.Identifier {
			t.Errorf("%s: root differs", c.Name)
		}
		if len(b.Blobs) != len(c.BlobHashesHex) || len(b.Branches) != len(c.BranchesHex) {
			t.Fatalf("%s: %d blobs, %d branches", c.Name, len(b.Blobs), len(b.Branches))
		}
		objects := map[[32]byte][]byte{b.RootHash: b.Root}
		for i, bl := range b.Blobs {
			if sha256.Sum256(bl) != h32(t, c.BlobHashesHex[i]) {
				t.Errorf("%s: blob %d differs", c.Name, i)
			}
			objects[sha256.Sum256(bl)] = bl
		}
		for i, br := range b.Branches {
			if hex.EncodeToString(br) != c.BranchesHex[i] {
				t.Errorf("%s: branch %d differs", c.Name, i)
			}
			objects[sha256.Sum256(br)] = br
		}
		// The streaming builder emits the same blobs and root.
		var emitted [][32]byte
		ch := NewChunker(func(blob []byte) error { emitted = append(emitted, sha256.Sum256(blob)); return nil })
		for off := 0; off < len(content); off += 1 << 20 {
			ch.Write(content[off:min(off+1<<20, len(content))])
		}
		sb, err := ch.Finish(nil)
		if err != nil || sb.RootHash != b.RootHash || len(emitted) != len(b.Blobs) {
			t.Errorf("%s: chunker differs: %v", c.Name, err)
		}
		cl, err := Verify(b.RootHash, func(h [32]byte) ([]byte, error) {
			if o, ok := objects[h]; ok {
				return o, nil
			}
			return nil, errors.New("none")
		}, Limits{})
		if err != nil || !cl.Canonical || len(cl.Objects) != c.Objects || len(cl.Blobs) != len(b.Blobs) {
			t.Errorf("%s: verify %v", c.Name, err)
		}
	}
}

func TestShapes(t *testing.T) {
	for _, s := range load(t).Shapes {
		refs := make([]Child, s.Leaves)
		for i := range refs {
			refs[i] = Child{Kind: ChildBlob, Length: ChunkSize, Hash: sha256.Sum256([]byte("BRC-167 canonical tree leaf:" + itoa(i)))}
		}
		b, err := BuildTree(refs, h32(t, s.ContentHashHex), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(b.Branches) != s.Branches || b.RootHash != h32(t, s.RootHashHex) {
			t.Errorf("%d leaves: %d branches, root %x", s.Leaves, len(b.Branches), b.RootHash)
		}
		r, _ := DecodeRoot(b.Root)
		if len(r.Children) != len(s.RootChildren) {
			t.Fatalf("%d leaves: %d root children", s.Leaves, len(r.Children))
		}
		for i, c := range s.RootChildren {
			if r.Children[i] != (Child{c.Kind, c.Length, h32(t, c.HashHex)}) {
				t.Errorf("%d leaves: root child %d differs", s.Leaves, i)
			}
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

func TestNodes(t *testing.T) {
	for _, n := range load(t).Nodes {
		b := unhex(t, n.Hex)
		var err error
		if n.As == "root" {
			var r Root
			r, err = DecodeRoot(b)
			if err == nil {
				again, e := EncodeRoot(r)
				if e != nil || !bytes.Equal(again, b) {
					t.Errorf("%s: re-encode differs: %v", n.Name, e)
				}
			}
		} else {
			var br Branch
			br, err = DecodeBranch(b)
			if err == nil {
				again, e := EncodeBranch(br)
				if e != nil || !bytes.Equal(again, b) {
					t.Errorf("%s: re-encode differs: %v", n.Name, e)
				}
			}
		}
		if got := reason(err); got != n.Reason {
			t.Errorf("%s: %q, want %q", n.Name, got, n.Reason)
		}
	}
}

func TestClosures(t *testing.T) {
	for _, c := range load(t).Closures {
		objects := map[[32]byte][]byte{}
		for _, o := range c.Objects {
			objects[h32(t, o.HashHex)] = unhex(t, o.Hex)
		}
		cl, err := Verify(h32(t, c.RootHashHex), func(h [32]byte) ([]byte, error) {
			if o, ok := objects[h]; ok {
				return o, nil
			}
			return nil, errors.New("none")
		}, Limits{MaxReferences: c.MaxReferences, MaxLength: c.MaxLength})
		if got := reason(err); got != c.Reason {
			t.Errorf("%s: %q, want %q", c.Name, got, c.Reason)
			continue
		}
		if err == nil && (cl.Canonical != c.Canonical || len(cl.Blobs) != c.Blobs) {
			t.Errorf("%s: canonical %v, %d blobs", c.Name, cl.Canonical, len(cl.Blobs))
		}
	}
}

func TestIdentifiers(t *testing.T) {
	for _, c := range load(t).Identifiers {
		var h [32]byte
		var err error
		if c.URL {
			h, err = ParseURL(c.Input)
		} else {
			h, err = ParseIdentifier(c.Input)
		}
		if got := reason(err); got != c.Reason {
			t.Errorf("%s: %q, want %q", c.Name, got, c.Reason)
			continue
		}
		if err == nil && h != h32(t, c.HashHex) {
			t.Errorf("%s: hash %x", c.Name, h)
		}
	}
}

// The encoder refuses what the decoder refuses.
func TestEncodeRefuses(t *testing.T) {
	hh := sha256.Sum256([]byte("hello"))
	one := []Child{{ChildBlob, 5, hh}}
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"profile 0", second(EncodeRoot(Root{Length: 5, ContentHash: hh, Children: one})), ErrProfile},
		{"sum", second(EncodeRoot(Root{Profile: 1, Length: 6, ContentHash: hh, Children: one})), ErrLength},
		{"empty", second(EncodeRoot(Root{Profile: 1, ContentHash: hh})), ErrEmpty},
		{"kind", second(EncodeRoot(Root{Profile: 1, Length: 5, ContentHash: hh, Children: []Child{{2, 5, hh}}})), ErrChildKind},
		{"critical", second(EncodeRoot(Root{Profile: 1, Length: 5, ContentHash: hh, Children: one, Extensions: []Extension{{Type: 4}}})), ErrExtension},
		{"no children", second(EncodeBranch(Branch{})), ErrChildCount},
		{"mediaType on a branch", second(EncodeBranch(Branch{Length: 5, Children: one, Extensions: []Extension{{1, []byte("a/b")}}})), ErrExtension},
		{"overflow", second(EncodeBranch(Branch{Length: 4, Children: []Child{{0, 1 << 63, hh}, {0, 1 << 63, hh}, {0, 4, hh}}})), ErrLength},
	}
	for _, c := range cases {
		if !errors.Is(c.err, c.want) {
			t.Errorf("%s: %v", c.name, c.err)
		}
	}
	if _, err := BuildTree([]Child{{ChildBlob, 5, hh}, {ChildBlob, 5, hh}}, hh, nil); !errors.Is(err, ErrCanonical) {
		t.Errorf("short first blob: %v", err)
	}
	stop := errors.New("stop")
	ch := NewChunker(func([]byte) error { return stop })
	if _, err := ch.Write(make([]byte, ChunkSize)); err != stop {
		t.Errorf("emit error: %v", err)
	}
	if _, err := ch.Finish(nil); err != stop {
		t.Errorf("finish after an emit error: %v", err)
	}
}

func second(_ []byte, err error) error { return err }

func TestMediaType(t *testing.T) {
	for s, ok := range map[string]bool{
		"text/plain": true, "application/vnd.example+json": true, "a/b": true, "video/mp4": true,
		"ab": false, "text/": false, "/plain": false, "text/plain/x": false, "Text/plain": false,
		"text/plain; charset=x": false, "text/.plain": false, "+text/plain": false,
	} {
		if ValidMediaType(s) != ok {
			t.Errorf("%q: %v", s, !ok)
		}
	}
}

// Every truncation and every single-byte change of a valid root is either
// refused with a named reason or decodes to something that re-encodes to the
// same bytes.
func FuzzDecodeRoot(f *testing.F) {
	b, _ := Build([]byte("hello"), []Extension{{1, []byte("text/plain")}})
	f.Add(b.Root)
	f.Fuzz(func(t *testing.T, in []byte) {
		r, err := DecodeRoot(in)
		if err != nil {
			if _, ok := Reason(err); !ok {
				t.Fatalf("unnamed refusal %v", err)
			}
			return
		}
		out, err := EncodeRoot(r)
		if err != nil || !bytes.Equal(out, in) {
			t.Fatalf("re-encode: %v", err)
		}
	})
}

func TestEveryChange(t *testing.T) {
	b, _ := Build([]byte("hello"), []Extension{{1, []byte("text/plain")}})
	check := func(in []byte) {
		r, err := DecodeRoot(in)
		if err != nil {
			if _, ok := Reason(err); !ok {
				t.Fatalf("unnamed refusal %v", err)
			}
			return
		}
		if out, err := EncodeRoot(r); err != nil || !bytes.Equal(out, in) {
			t.Fatalf("re-encode of %x: %v", in, err)
		}
	}
	for i := range b.Root {
		check(b.Root[:i])
		for _, x := range []byte{0, 1, 0x7f, 0xfd, 0xff} {
			c := append([]byte(nil), b.Root...)
			c[i] = x
			check(c)
		}
	}
}
