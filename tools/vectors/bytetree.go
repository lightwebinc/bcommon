package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// The byte-leaf family: RFC 6962 over leaves of any length, with this
// generator's own mth, auditPath and verifyPath (rfc6962.go), and the same
// construction over content cut into 4,096-byte segments.

// streamDomain names the deterministic content both families cut up.
const streamDomain = "bcommon vector content"

// stream is n bytes of SHA-256(streamDomain || uint32be(j)) for j = 0, 1, ...
// concatenated: fixed, incompressible, and rebuilt by any reader from the
// rule alone.
func stream(n int) []byte {
	out := make([]byte, 0, n+32)
	var j uint32
	for len(out) < n {
		b := binary.BigEndian.AppendUint32([]byte(streamDomain), j)
		h := sha256.Sum256(b)
		out = append(out, h[:]...)
		j++
	}
	return out[:n]
}

type byteLeaf struct {
	Hex string `json:"hex"`
}

type segmentSample struct {
	Index       int      `json:"index"`
	PathLen     int      `json:"pathLen"`
	SiblingsHex []string `json:"siblingsHex"`
}

type segmentObject struct {
	Size     int             `json:"size"`
	Segments int             `json:"segments"`
	RootHex  string          `json:"rootHex"`
	Samples  []segmentSample `json:"samples"`
}

type byteTreeVector struct {
	Note        string          `json:"note"`
	Leaves      []byteLeaf      `json:"leaves"`
	Trees       []tree          `json:"trees"`
	SegmentSize int             `json:"segmentSize"`
	Content     string          `json:"content"`
	Objects     []segmentObject `json:"objects"`
}

func byteTreeFamilies() ([]family, error) {
	v := byteTreeVector{
		Note:        "trees[i] is over the first i leaves, from 0; objects are stream(size) cut into segments of segmentSize bytes, the last unpadded; samples carry the audit path's hashes leaf to root without sides",
		SegmentSize: 4096,
		Content:     "SHA-256(\"" + streamDomain + "\" || uint32be(j)) for j = 0, 1, ... concatenated, cut to size",
	}
	lengths := []int{0, 1, 2, 31, 32, 33, 63, 64, 65, 100, 255, 4096}
	var leaves [][]byte
	s := stream(8192)
	off := 0
	for _, n := range lengths {
		leaves = append(leaves, s[off:off+n])
		off += n
		v.Leaves = append(v.Leaves, byteLeaf{Hex: hex.EncodeToString(leaves[len(leaves)-1])})
	}
	for n := 0; n <= len(leaves); n++ {
		d := leaves[:n]
		root := mth(d)
		t := tree{Size: n, RootHex: hex.EncodeToString(root[:]), Paths: []inclusion{}}
		for m := 0; m < n; m++ {
			p := auditPath(m, d)
			if err := verifyPath(m, n, d[m], p, root); err != nil {
				return nil, fmt.Errorf("bytetree: n=%d m=%d: %w", n, m, err)
			}
			in := inclusion{Index: m, Steps: []step{}}
			for _, st := range p {
				in.Steps = append(in.Steps, step{HashHex: hex.EncodeToString(st.hash[:]), Left: st.left})
			}
			t.Paths = append(t.Paths, in)
		}
		v.Trees = append(v.Trees, t)
	}
	// An empty tree's root is SHA-256 of nothing.
	if v.Trees[0].RootHex != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		return nil, fmt.Errorf("bytetree: empty root %s", v.Trees[0].RootHex)
	}
	for _, size := range []int{0, 1, 4095, 4096, 4097, 3*4096 + 5, 4194304, 4194305} {
		c := stream(size)
		var segs [][]byte
		for o := 0; o < size; o += v.SegmentSize {
			segs = append(segs, c[o:min(o+v.SegmentSize, size)])
		}
		root := mth(segs)
		obj := segmentObject{Size: size, Segments: len(segs), RootHex: hex.EncodeToString(root[:]), Samples: []segmentSample{}}
		n := len(segs)
		idx := map[int]bool{}
		for _, i := range []int{0, n / 2, n - 2, n - 1} {
			if i < 0 || i >= n || idx[i] {
				continue
			}
			idx[i] = true
			p := auditPath(i, segs)
			if err := verifyPath(i, n, segs[i], p, root); err != nil {
				return nil, fmt.Errorf("bytetree: size=%d i=%d: %w", size, i, err)
			}
			sm := segmentSample{Index: i, PathLen: len(p), SiblingsHex: []string{}}
			for _, st := range p {
				sm.SiblingsHex = append(sm.SiblingsHex, hex.EncodeToString(st.hash[:]))
			}
			obj.Samples = append(obj.Samples, sm)
		}
		v.Objects = append(v.Objects, obj)
	}
	return []family{{"bytetree-v1.json", v}}, nil
}
