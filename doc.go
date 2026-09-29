// Package bcommon is the root of a library of building blocks for BSV
// overlay applications that publish and verify committed records: a
// deterministic CBOR codec, RFC 6962 roots, store references and manifests,
// BRC-42/43 derivation and tagged PushDrop outputs, a non-final carrier that
// commits a payload to the chain, transaction builders with a fee loop,
// funding-tree state, node, arcade and wallet clients, the producer's
// orchestration of fees, funding trees and proofs around them, a header
// source, host sets with quorum fan-out, BRC-24 lookup, BRC-169 and BRC-180
// resolution, a pinned-key file, SPV verdicts, and a filter for text that
// reaches a terminal. Each subdirectory that holds Go code is one package;
// this root package holds no code of its own.
//
// An application supplies what makes these packages its own: the payload
// schema, the derivation protocol and key ids, the output tags, the wallet
// profile, the RPC id and the pin file's header all arrive as parameters
// rather than defaults. A default derivation or wallet profile would quietly
// re-key an application that forgot to pass one.
//
// testdata is not a package. Its fixtures directory holds response bodies
// vendored from the services the network packages talk to, and its vectors
// directory the vectors an independent generator (tools/vectors) writes,
// which the packages' tests compare their own output against byte for byte.
// Both are kept at the root so every package reads them by the same
// relative path.
//
// # Imports
//
// Every package here, in production code and in tests, imports only the
// standard library, github.com/bsv-blockchain/go-sdk and other packages of
// this module. One outside dependency is what an application pinning this
// library takes on, so a second one would be a cost to every application at
// once. TestBoundary and TestBoundaryFiles enforce the rule. The vector
// generator in tools/vectors is a separate module that nothing here imports,
// held to a rule of its own: it may add an independent CBOR encoder, and it
// may never import this module.
//
// Nor does any package take on what belongs to an application's command:
// flags, logging, the environment, the user's configuration directories,
// the standard streams, other processes or the process's exit. Settings and
// output arrive as parameters. TestNoProcessConcerns enforces it.
//
// # Versioning
//
// The library is pre-1.0. An application pins an exact tag and moves to a
// newer one deliberately, because a v0 minor version may change the API.
package bcommon
