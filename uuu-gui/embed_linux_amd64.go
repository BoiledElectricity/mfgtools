//go:build linux && amd64

package main

import _ "embed"

//go:embed bins/linux_amd64/uuu
var uuuBin []byte

const uuuExeName = "uuu"
