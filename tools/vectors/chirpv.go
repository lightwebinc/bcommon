package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
)

// The CHIRP family: BRC-167 version 1 nodes and profile 1 closures, written
// here from the BRC's tables with nothing shared with the library. Every
// golden value BRC-167 prints (three roots, their hashes and identifiers,
// and the tree shapes over 256, 257 and 65,537 synthetic blobs) is
// reproduced before anything is written, and the generator fails if one
// stops matching. Refusal cases are labelled by their construction: each
// breaks one rule of a valid node or closure.

const chirpChunk = 4194304

type cref struct {
	kind   byte
	length uint64
	hash   [32]byte
}

type cext struct {
	typ uint64
	val []byte
}

func compactSize(v uint64) []byte {
	switch {
	case v <= 252:
		return []byte{byte(v)}
	case v <= 0xffff:
		b := []byte{0xfd, 0, 0}
		binary.LittleEndian.PutUint16(b[1:], uint16(v))
		return b
	case v <= 0xffffffff:
		b := []byte{0xfe, 0, 0, 0, 0}
		binary.LittleEndian.PutUint32(b[1:], uint32(v))
		return b
	}
	b := make([]byte, 9)
	b[0] = 0xff
	binary.LittleEndian.PutUint64(b[1:], v)
	return b
}

func nodeTail(w *bytes.Buffer, refs []cref, ext []cext) {
	w.Write(compactSize(uint64(len(refs))))
	for _, r := range refs {
		w.WriteByte(r.kind)
		binary.Write(w, binary.BigEndian, r.length)
		w.Write(r.hash[:])
	}
	w.Write(compactSize(uint64(len(ext))))
	for _, e := range ext {
		w.Write(compactSize(e.typ))
		w.Write(compactSize(uint64(len(e.val))))
		w.Write(e.val)
	}
}

// rootNode writes a root with any field values, valid or not.
func rootNode(major, minor byte, profile uint16, length uint64, content [32]byte, refs []cref, ext []cext) []byte {
	var w bytes.Buffer
	w.WriteString("CHIRP")
	w.Write([]byte{major, minor, 0})
	binary.Write(&w, binary.BigEndian, profile)
	binary.Write(&w, binary.BigEndian, length)
	w.Write(content[:])
	nodeTail(&w, refs, ext)
	return w.Bytes()
}

func branchNode(length uint64, refs []cref, ext []cext) []byte {
	var w bytes.Buffer
	w.WriteString("CHIRP")
	w.Write([]byte{1, 0, 1})
	binary.Write(&w, binary.BigEndian, length)
	nodeTail(&w, refs, ext)
	return w.Bytes()
}

func sumLen(refs []cref) uint64 {
	var s uint64
	for _, r := range refs {
		s += r.length
	}
	return s
}

// canonical is profile 1's steps 5 to 7: group the references 256 at a time
// until at most 256 remain. It returns the root's children, the branches
// written, and the width of each level, leaves first.
func canonical(refs []cref) ([]cref, [][]byte, []int) {
	widths := []int{len(refs)}
	var branches [][]byte
	for len(refs) > 256 {
		var next []cref
		for i := 0; i < len(refs); i += 256 {
			g := refs[i:min(i+256, len(refs))]
			b := branchNode(sumLen(g), g, nil)
			branches = append(branches, b)
			next = append(next, cref{1, sumLen(g), sha256.Sum256(b)})
		}
		refs = next
		widths = append(widths, len(refs))
	}
	return refs, branches, widths
}

func blobRefs(content []byte) ([]cref, [][]byte) {
	var refs []cref
	var blobs [][]byte
	for o := 0; o < len(content); o += chirpChunk {
		b := content[o:min(o+chirpChunk, len(content))]
		blobs = append(blobs, b)
		refs = append(refs, cref{0, uint64(len(b)), sha256.Sum256(b)})
	}
	return refs, blobs
}

const b58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// base58check over math/big, the leading zero bytes as '1's.
func base58check(payload []byte) string {
	s1 := sha256.Sum256(payload)
	s2 := sha256.Sum256(s1[:])
	b := append(append([]byte(nil), payload...), s2[:4]...)
	x := new(big.Int).SetBytes(b)
	var out []byte
	m := new(big.Int)
	for x.Sign() > 0 {
		x.DivMod(x, big.NewInt(58), m)
		out = append(out, b58[m.Int64()])
	}
	for _, c := range b {
		if c != 0 {
			break
		}
		out = append(out, '1')
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

func objectID(h [32]byte) string { return base58check(append([]byte{0xce, 0x00}, h[:]...)) }

type extJSON struct {
	Type     uint64 `json:"type"`
	ValueHex string `json:"valueHex"`
}

type chirpGolden struct {
	Name           string    `json:"name"`
	ContentHex     string    `json:"contentHex"`
	Extensions     []extJSON `json:"extensions"`
	ContentHashHex string    `json:"contentHashHex"`
	RootHex        string    `json:"rootHex"`
	RootHashHex    string    `json:"rootHashHex"`
	Identifier     string    `json:"identifier"`
	URL            string    `json:"url"`
}

// contentPart is a run of the stream (stream(n)) or of literal bytes; a
// content case is its parts concatenated.
type contentPart struct {
	Stream int    `json:"stream,omitempty"`
	Hex    string `json:"hex,omitempty"`
}

type chirpContent struct {
	Name           string        `json:"name"`
	Parts          []contentPart `json:"parts"`
	Size           int           `json:"size"`
	ContentHashHex string        `json:"contentHashHex"`
	BlobHashesHex  []string      `json:"blobHashesHex"`
	BranchesHex    []string      `json:"branchesHex"`
	RootHex        string        `json:"rootHex"`
	RootHashHex    string        `json:"rootHashHex"`
	Identifier     string        `json:"identifier"`
	// Objects is the distinct object count: root, branches and blobs.
	Objects int `json:"objects"`
}

type childJSON struct {
	Kind    byte   `json:"kind"`
	Length  uint64 `json:"length"`
	HashHex string `json:"hashHex"`
}

type chirpShape struct {
	Leaves         int         `json:"leaves"`
	Widths         []int       `json:"widths"`
	Branches       int         `json:"branches"`
	ContentHashHex string      `json:"contentHashHex"`
	RootChildren   []childJSON `json:"rootChildren"`
	RootHashHex    string      `json:"rootHashHex"`
}

type chirpNodeCase struct {
	Name string `json:"name"`
	// As is the decoder the bytes are given to: root or branch.
	As     string `json:"as"`
	Hex    string `json:"hex"`
	Reason string `json:"reason"`
}

type objectJSON struct {
	HashHex string `json:"hashHex"`
	Hex     string `json:"hex"`
}

type chirpClosureCase struct {
	Name          string       `json:"name"`
	RootHashHex   string       `json:"rootHashHex"`
	Objects       []objectJSON `json:"objects"`
	MaxReferences int          `json:"maxReferences"`
	MaxLength     uint64       `json:"maxLength"`
	Reason        string       `json:"reason"`
	Canonical     bool         `json:"canonical"`
	// Blobs is the number of blob references in content order, on a pass.
	Blobs int `json:"blobs"`
}

type chirpIDCase struct {
	Name    string `json:"name"`
	Input   string `json:"input"`
	URL     bool   `json:"url"`
	HashHex string `json:"hashHex"`
	Reason  string `json:"reason"`
}

type chirpVector struct {
	Note       string             `json:"note"`
	ChunkSize  int                `json:"chunkSize"`
	Content    string             `json:"content"`
	Golden     []chirpGolden      `json:"golden"`
	Contents   []chirpContent     `json:"contents"`
	Shapes     []chirpShape       `json:"shapes"`
	Nodes      []chirpNodeCase    `json:"nodes"`
	Closures   []chirpClosureCase `json:"closures"`
	Identities []chirpIDCase      `json:"identifiers"`
}

func hx(b []byte) string { return hex.EncodeToString(b) }

func chirpFamilies() ([]family, error) {
	v := chirpVector{
		Note:      "BRC-167 version 1 and profile 1; golden roots and shapes are the BRC's own; node and closure refusals name the first rule broken",
		ChunkSize: chirpChunk,
		Content:   "a stream part is SHA-256(\"" + streamDomain + "\" || uint32be(j)) for j = 0, 1, ... concatenated, cut to its length",
	}
	empty := sha256.Sum256(nil)

	// BRC-167's three minimal golden vectors.
	hello := []byte("hello")
	hh := sha256.Sum256(hello)
	golden := []struct {
		name, content, ext, root, hash, id string
	}{
		{"empty", "", "", "434849525001000000010000000000000000e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b8550000", "0403640d635fd27b6c719d2b81db853483ff8d0fb46f7b90ce9f9d7e9a2729ee", "XUSvYkywHxEMvs7oiYYMV8bJ1sJjHq2mHgZvu8jSLyLhbNRVjG8E"},
		{"hello", "hello", "", "4348495250010000000100000000000000052cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824010000000000000000052cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b982400", "1731ac8562f744fdd7990a5ef69b36bc140761db5b0e2936e896021d03b276a9", "XUT4zhwjYd9NLrcGUudTnMiQ7WpA3SeEa4T7ZwrcNn85qmW1XucC"},
		{"hello text/plain", "hello", "text/plain", "4348495250010000000100000000000000052cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824010000000000000000052cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b982401010a746578742f706c61696e", "5493c139e9366f7c3facf9b3f28d5e0da6514fec975e70f59f5b2c3a40cd2c85", "XUTY2f2HxHyj7RDPgsSngBETiwZj58oYfjyGgfgFLsCE2y3mgrGv"},
	}
	if objectID(hh) != "XUTEaLuvEhPySbAMiJxYEhBBGb258URNoqgnaf3Ym4b2wg683ZKp" {
		return nil, fmt.Errorf("chirp: blob identifier of hello is %s", objectID(hh))
	}
	for _, g := range golden {
		c := []byte(g.content)
		refs, _ := blobRefs(c)
		var ext []cext
		ej := []extJSON{}
		if g.ext != "" {
			ext = []cext{{1, []byte(g.ext)}}
			ej = append(ej, extJSON{1, hx([]byte(g.ext))})
		}
		ch := sha256.Sum256(c)
		r := rootNode(1, 0, 1, uint64(len(c)), ch, refs, ext)
		rh := sha256.Sum256(r)
		if hx(r) != g.root || hx(rh[:]) != g.hash || objectID(rh) != g.id {
			return nil, fmt.Errorf("chirp: golden %q does not reproduce BRC-167", g.name)
		}
		v.Golden = append(v.Golden, chirpGolden{g.name, hx(c), ej, hx(ch[:]), g.root, g.hash, g.id, "chirp://" + g.id})
	}

	// Closures built from content.
	contents := []struct {
		name  string
		parts []contentPart
	}{
		{"empty", nil},
		{"one byte", []contentPart{{Stream: 1}}},
		{"chunk minus one", []contentPart{{Stream: chirpChunk - 1}}},
		{"one chunk", []contentPart{{Stream: chirpChunk}}},
		{"chunk plus one", []contentPart{{Stream: chirpChunk + 1}}},
		{"repeated blob", []contentPart{{Stream: chirpChunk}, {Stream: chirpChunk}, {Hex: "7461696c"}}},
	}
	for _, cc := range contents {
		var c []byte
		for _, p := range cc.parts {
			if p.Hex != "" {
				b, _ := hex.DecodeString(p.Hex)
				c = append(c, b...)
			} else {
				c = append(c, stream(p.Stream)...)
			}
		}
		refs, _ := blobRefs(c)
		children, branches, _ := canonical(refs)
		ch := sha256.Sum256(c)
		r := rootNode(1, 0, 1, uint64(len(c)), ch, children, nil)
		rh := sha256.Sum256(r)
		out := chirpContent{Name: cc.name, Parts: cc.parts, Size: len(c), ContentHashHex: hx(ch[:]), BlobHashesHex: []string{}, BranchesHex: []string{}, RootHex: hx(r), RootHashHex: hx(rh[:]), Identifier: objectID(rh)}
		if out.Parts == nil {
			out.Parts = []contentPart{}
		}
		distinct := map[[32]byte]bool{rh: true}
		for _, b := range branches {
			out.BranchesHex = append(out.BranchesHex, hx(b))
			distinct[sha256.Sum256(b)] = true
		}
		for _, r := range refs {
			out.BlobHashesHex = append(out.BlobHashesHex, hx(r.hash[:]))
			distinct[r.hash] = true
		}
		out.Objects = len(distinct)
		v.Contents = append(v.Contents, out)
	}

	// BRC-167's compact tree shapes.
	shapes := []struct {
		leaves   int
		widths   []int
		branches int
		children []string
	}{
		{256, []int{256}, 0, nil},
		{257, []int{257, 2}, 2, []string{"1073741824:65f0e211bb73fbe7c7db0a0433d1626dc1c775821fa658bbf43175e610e44fa2", "4194304:bc57e1fe69cbc6245cace01bbd61591cf81737a162707dea131b63da3f77e74e"}},
		{65537, []int{65537, 257, 2}, 259, []string{"274877906944:9b196daab8cd5ef832093f9fadab3656e3cc2fd2256857e91956f4baacd0a0e8", "4194304:cfee311fdfa2f73242c729513a301e0b670b3bd6511bce9c2f65018a2da97c53"}},
	}
	shapeContent := sha256.Sum256([]byte("bcommon chirp shape content"))
	for _, s := range shapes {
		refs := make([]cref, s.leaves)
		for i := range refs {
			refs[i] = cref{0, chirpChunk, sha256.Sum256([]byte("BRC-167 canonical tree leaf:" + strconv.Itoa(i)))}
		}
		children, branches, widths := canonical(refs)
		if fmt.Sprint(widths) != fmt.Sprint(s.widths) || len(branches) != s.branches {
			return nil, fmt.Errorf("chirp: shape %d: widths %v, %d branches", s.leaves, widths, len(branches))
		}
		if s.children != nil {
			if len(children) != len(s.children) {
				return nil, fmt.Errorf("chirp: shape %d: %d root children", s.leaves, len(children))
			}
			for i, want := range s.children {
				if got := fmt.Sprintf("%d:%s", children[i].length, hx(children[i].hash[:])); got != want {
					return nil, fmt.Errorf("chirp: shape %d child %d: %s", s.leaves, i, got)
				}
			}
		}
		r := rootNode(1, 0, 1, sumLen(refs), shapeContent, children, nil)
		rh := sha256.Sum256(r)
		sh := chirpShape{Leaves: s.leaves, Widths: widths, Branches: len(branches), ContentHashHex: hx(shapeContent[:]), RootHashHex: hx(rh[:])}
		for _, c := range children {
			sh.RootChildren = append(sh.RootChildren, childJSON{c.kind, c.length, hx(c.hash[:])})
		}
		v.Shapes = append(v.Shapes, sh)
	}

	// Node refusals, each one rule broken.
	helloRef := cref{0, 5, hh}
	helloRoot := rootNode(1, 0, 1, 5, hh, []cref{helloRef}, nil)
	helloBranch := branchNode(5, []cref{helloRef}, nil)
	world := sha256.Sum256([]byte("world"))
	mt := func(s string) []cext { return []cext{{1, []byte(s)}} }
	many := make([]cref, 257)
	for i := range many {
		many[i] = cref{0, 1, hh}
	}
	set := func(b []byte, i int, x byte) []byte { c := append([]byte(nil), b...); c[i] = x; return c }
	// childCount at offset 50 in a root: 5 magic, 3 versions and kind, 2
	// profile, 8 length, 32 content hash.
	nonMinimal := append(append(append([]byte(nil), helloRoot[:50]...), 0xfd, 0x01, 0x00), helloRoot[51:]...)
	nodes := []struct {
		name, as string
		b        []byte
		reason   string
	}{
		{"hello root", "root", helloRoot, ""},
		{"empty root", "root", rootNode(1, 0, 1, 0, empty, nil, nil), ""},
		{"mediaType", "root", rootNode(1, 0, 1, 5, hh, []cref{helloRef}, mt("application/vnd.example+json")), ""},
		{"unknown advisory extension", "root", rootNode(1, 0, 1, 5, hh, []cref{helloRef}, []cext{{1, []byte("text/plain")}, {3, []byte{1, 2, 3}}}), ""},
		{"branch", "branch", helloBranch, ""},
		{"branch of 256", "branch", branchNode(256, many[:256], nil), ""},
		{"too large", "root", append(append([]byte(nil), helloRoot...), make([]byte, 65537-len(helloRoot))...), "node-size"},
		{"short header", "root", []byte("CHIRP\x01\x00"), "truncated"},
		{"cut last byte", "root", helloRoot[:len(helloRoot)-1], "truncated"},
		{"cut child", "branch", helloBranch[:40], "truncated"},
		{"magic", "root", set(helloRoot, 4, 'Q'), "magic"},
		{"major 2", "root", set(helloRoot, 5, 2), "version"},
		{"minor 1", "root", set(helloRoot, 6, 1), "version"},
		{"node kind 2", "root", set(helloRoot, 7, 2), "node-kind"},
		{"branch as root", "root", helloBranch, "node-kind"},
		{"root as branch", "branch", helloRoot, "node-kind"},
		{"profile 0", "root", rootNode(1, 0, 0, 5, hh, []cref{helloRef}, nil), "profile"},
		{"non-minimal count", "root", nonMinimal, "compact-size"},
		{"257 children", "root", rootNode(1, 0, 1, 257, hh, many, nil), "child-count"},
		{"branch of 257", "branch", branchNode(257, many, nil), "child-count"},
		{"branch of none", "branch", branchNode(0, nil, nil), "child-count"},
		{"child kind 2", "root", rootNode(1, 0, 1, 5, hh, []cref{{2, 5, hh}}, nil), "child-kind"},
		{"root length", "root", rootNode(1, 0, 1, 6, hh, []cref{helloRef}, nil), "length"},
		{"branch length", "branch", branchNode(4, []cref{helloRef}, nil), "length"},
		{"empty with a child", "root", rootNode(1, 0, 1, 0, empty, []cref{{0, 0, empty}}, nil), "empty"},
		{"empty content hash", "root", rootNode(1, 0, 1, 0, world, nil, nil), "empty"},
		{"trailing byte", "root", append(append([]byte(nil), helloRoot...), 0), "trailing"},
		{"unsorted extensions", "root", rootNode(1, 0, 1, 5, hh, []cref{helloRef}, []cext{{3, nil}, {1, []byte("text/plain")}}), "extension"},
		{"duplicate extension", "root", rootNode(1, 0, 1, 5, hh, []cref{helloRef}, []cext{{3, nil}, {3, nil}}), "extension"},
		{"extension type 0", "root", rootNode(1, 0, 1, 5, hh, []cref{helloRef}, []cext{{0, nil}}), "extension"},
		{"unknown critical extension", "root", rootNode(1, 0, 1, 5, hh, []cref{helloRef}, []cext{{2, nil}}), "extension"},
		{"mediaType on a branch", "branch", branchNode(5, []cref{helloRef}, mt("text/plain")), "extension"},
		{"mediaType upper case", "root", rootNode(1, 0, 1, 5, hh, []cref{helloRef}, mt("Text/plain")), "extension"},
		{"mediaType parameter", "root", rootNode(1, 0, 1, 5, hh, []cref{helloRef}, mt("text/plain;charset=utf-8")), "extension"},
		{"mediaType no subtype", "root", rootNode(1, 0, 1, 5, hh, []cref{helloRef}, mt("text/")), "extension"},
		{"extension values too long", "root", rootNode(1, 0, 1, 5, hh, []cref{helloRef}, []cext{{3, make([]byte, 16385)}}), "extension"},
	}
	for _, n := range nodes {
		v.Nodes = append(v.Nodes, chirpNodeCase{n.name, n.as, hx(n.b), n.reason})
	}

	// Closure refusals.
	type obj struct {
		h [32]byte
		b []byte
	}
	o := func(b []byte) obj { return obj{sha256.Sum256(b), b} }
	helloRootObj := o(helloRoot)
	worldRef := cref{0, 5, world}
	twoBlobs := rootNode(1, 0, 1, 10, sha256.Sum256([]byte("helloworld")), []cref{helloRef, worldRef}, nil)
	twoBlobs2 := rootNode(1, 0, 2, 10, sha256.Sum256([]byte("helloworld")), []cref{helloRef, worldRef}, nil)
	viaBranch := rootNode(1, 0, 1, 5, hh, []cref{{1, 5, sha256.Sum256(helloBranch)}}, nil)
	longBranchRef := rootNode(1, 0, 1, 6, hh, []cref{{1, 6, sha256.Sum256(helloBranch)}}, nil)
	badBranch := branchNode(5, []cref{helloRef}, mt("text/plain"))
	viaBadBranch := rootNode(1, 0, 2, 5, hh, []cref{{1, 5, sha256.Sum256(badBranch)}}, nil)
	// A chain of k branches over hello: the path from the root to the blob
	// holds k + 2 objects.
	chain := func(k int, profile uint16) (obj, []obj) {
		var objs []obj
		ref := helloRef
		for i := 0; i < k; i++ {
			b := branchNode(5, []cref{ref}, nil)
			objs = append(objs, o(b))
			ref = cref{1, 5, sha256.Sum256(b)}
		}
		return o(rootNode(1, 0, profile, 5, hh, []cref{ref}, nil)), objs
	}
	deepRoot, deepObjs := chain(15, 2)
	okDeepRoot, okDeepObjs := chain(14, 2)
	world5 := o([]byte("world"))
	helloObj := o(hello)
	closures := []struct {
		name      string
		root      [32]byte
		objs      []obj
		maxRefs   int
		maxLen    uint64
		reason    string
		canonical bool
		blobs     int
	}{
		{"hello", helloRootObj.h, []obj{helloRootObj, helloObj}, 0, 0, "", true, 1},
		{"empty", sha256.Sum256(rootNode(1, 0, 1, 0, empty, nil, nil)), []obj{o(rootNode(1, 0, 1, 0, empty, nil, nil))}, 0, 0, "", true, 0},
		{"profile 2 is not checked for construction", o(twoBlobs2).h, []obj{o(twoBlobs2), helloObj, world5}, 0, 0, "", false, 2},
		{"fourteen branches deep", okDeepRoot.h, append([]obj{okDeepRoot, helloObj}, okDeepObjs...), 0, 0, "", false, 1},
		{"root missing", helloRootObj.h, []obj{helloObj}, 0, 0, "missing", false, 0},
		{"root bytes wrong", helloRootObj.h, []obj{{helloRootObj.h, twoBlobs}, helloObj}, 0, 0, "hash", false, 0},
		{"root refused", o(set(helloRoot, 6, 1)).h, []obj{o(set(helloRoot, 6, 1)), helloObj}, 0, 0, "version", false, 0},
		{"too large", helloRootObj.h, []obj{helloRootObj, helloObj}, 0, 4, "too-large", false, 0},
		{"references", o(twoBlobs2).h, []obj{o(twoBlobs2), helloObj, world5}, 1, 0, "references", false, 0},
		{"fifteen branches deep", deepRoot.h, append([]obj{deepRoot, helloObj}, deepObjs...), 0, 0, "depth", false, 0},
		{"blob missing", helloRootObj.h, []obj{helloRootObj}, 0, 0, "missing", false, 0},
		{"blob bytes wrong", helloRootObj.h, []obj{helloRootObj, {hh, []byte("hellp")}}, 0, 0, "hash", false, 0},
		{"blob length wrong", helloRootObj.h, []obj{helloRootObj, {hh, []byte("hello!")}}, 0, 0, "length", false, 0},
		{"branch length wrong", o(longBranchRef).h, []obj{o(longBranchRef), o(helloBranch), helloObj}, 0, 0, "length", false, 0},
		{"branch refused", o(viaBadBranch).h, []obj{o(viaBadBranch), o(badBranch), helloObj}, 0, 0, "extension", false, 0},
		{"content hash", o(rootNode(1, 0, 1, 5, world, []cref{helloRef}, nil)).h, []obj{o(rootNode(1, 0, 1, 5, world, []cref{helloRef}, nil)), helloObj}, 0, 0, "content-hash", false, 0},
		{"short blob before the last", o(twoBlobs).h, []obj{o(twoBlobs), helloObj, world5}, 0, 0, "canonical", false, 0},
		{"needless branch", o(viaBranch).h, []obj{o(viaBranch), o(helloBranch), helloObj}, 0, 0, "canonical", false, 0},
	}
	for _, c := range closures {
		cc := chirpClosureCase{Name: c.name, RootHashHex: hx(c.root[:]), MaxReferences: c.maxRefs, MaxLength: c.maxLen, Reason: c.reason, Canonical: c.canonical, Blobs: c.blobs}
		for _, x := range c.objs {
			cc.Objects = append(cc.Objects, objectJSON{hx(x.h[:]), hx(x.b)})
		}
		v.Closures = append(v.Closures, cc)
	}

	// Identifiers and URLs.
	rh := sha256.Sum256(helloRoot)
	id := objectID(rh)
	flip := []byte(id)
	if flip[10] == 'a' {
		flip[10] = 'b'
	} else {
		flip[10] = 'a'
	}
	other := base58check(append([]byte{0xce, 0x01}, rh[:]...))
	short := base58check(append([]byte{0xce, 0x00}, rh[:31]...))
	ids := []chirpIDCase{
		{"identifier", id, false, hx(rh[:]), ""},
		{"empty root identifier", golden[0].id, false, golden[0].hash, ""},
		{"checksum", string(flip), false, "", "identifier"},
		{"prefix", other, false, "", "identifier"},
		{"short hash", short, false, "", "identifier"},
		{"not base58", "0" + id[1:], false, "", "identifier"},
		{"url", "chirp://" + id, true, hx(rh[:]), ""},
		{"compact url", "chirp:" + id, true, hx(rh[:]), ""},
		{"upper-case scheme", "CHIRP://" + id, true, hx(rh[:]), ""},
		{"url with a path", "chirp://" + id + "/x", true, "", "identifier"},
		{"url with a query", "chirp://" + id + "?a", true, "", "identifier"},
		{"uhrp url", "uhrp://" + id, true, "", "identifier"},
		{"bare identifier as url", id, true, "", "identifier"},
	}
	v.Identities = ids
	return []family{{"chirp-v1.json", v}}, nil
}
