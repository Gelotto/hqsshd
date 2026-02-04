package main

import (
	"os"

	"github.com/gelotto/hqsshd/internal/cli/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
