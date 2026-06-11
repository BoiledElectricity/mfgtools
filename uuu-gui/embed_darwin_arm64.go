//go:build darwin && arm64

package main

import _ "embed"

//go:embed bins/darwin_arm64/uuu
var uuuBin []byte

const uuuExeName = "uuu"
