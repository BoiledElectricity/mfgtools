//go:build !(darwin && arm64) && !(darwin && amd64) && !(linux && amd64) && !(linux && arm64) && !(windows && amd64)

package main

// Unsupported platform fallback: no embedded uuu. The -uuu flag still works.
var uuuBin []byte

const uuuExeName = "uuu"
