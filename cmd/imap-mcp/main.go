package main

import (
	"fmt"
	"os"

	"github.com/zjor/imap-mcp/internal/config"
)

func main() {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
	_ = cfg // the server is added in later tasks
}
