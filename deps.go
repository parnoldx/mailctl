//go:build tools

package main

// Pin backend dependencies so `go mod tidy` keeps them for the parallel
// IMAP/Graph implementations.
import (
	_ "github.com/BurntSushi/toml"
	_ "github.com/emersion/go-imap/v2"
	_ "github.com/emersion/go-message"
	_ "github.com/emersion/go-sasl"
	_ "github.com/emersion/go-smtp"
)
