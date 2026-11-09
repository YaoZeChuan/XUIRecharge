package web

import "embed"

// Files contains the user and administrator UI embedded in the server binary.
//
//go:embed user admin assets
var Files embed.FS
