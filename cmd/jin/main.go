package main

import (
	"os"

	"github.com/jin-k8s/jin/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
