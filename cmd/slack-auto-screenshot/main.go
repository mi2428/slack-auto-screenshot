package main

import (
	"fmt"
	"os"

	"github.com/mi2428/slack-auto-screenshot/internal/app"
)

func main() {
	err := app.NewCommand().Execute()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
