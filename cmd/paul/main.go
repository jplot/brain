package main

import (
	"os"

	"github.com/jplot/brain/internal/cli"
)

var version = "dev"

func main() {
	cli.Version = version
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
