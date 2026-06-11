//go:build windows && amd64

package main

import _ "embed"

//go:embed bins/windows_amd64/uuu.exe
var uuuBin []byte

const uuuExeName = "uuu.exe"
