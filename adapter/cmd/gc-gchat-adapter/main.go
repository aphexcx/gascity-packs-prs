// gc-gchat-adapter connects a city's external messages to Google Chat.
package main

import (
	"fmt"
	"os"
)

// Version is set by the build's -ldflags.
var Version = "dev"

func main() {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	switch command {
	case "version":
		fmt.Printf("gc-gchat-adapter %s\n", Version)
	case "serve", "auth", "state":
		fmt.Fprintln(os.Stderr, "not implemented yet")
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "usage: gc-gchat-adapter [serve|auth|state|version]")
		os.Exit(2)
	}
}
