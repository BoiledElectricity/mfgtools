//go:build linux && arm64

package main

import _ "embed"

//go:embed bins/linux_arm64/uuu
var uuuBin []byte

const uuuExeName = "uuu"
