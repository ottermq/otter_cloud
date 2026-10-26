// Package jaildeck is a small client for the JailDeck API over its unix
// socket. It runs in otter worker.
//
// Only this package knows JailDeck's API shape. otter api never loads
// its credentials.
//
// See docs/architecture.md §3.
package jaildeck
