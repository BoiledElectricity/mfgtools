//go:build darwin && amd64

package main

import _ "embed"

//go:embed bins/darwin_amd64/uuu
var uuuBin []byte

const uuuExeName = "uuu"
