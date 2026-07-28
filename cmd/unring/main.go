package main

import (
	"os"

	"github.com/hyj28/unring/internal/cli"
	"github.com/hyj28/unring/internal/ghshim"
)

func main() {
	if ghshim.IsInvocation(os.Args[0]) {
		os.Exit(ghshim.RunClient(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
