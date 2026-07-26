package main

import (
	"os"

	"github.com/HongyuanJia8/unring/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
