package main

import (
	"encoding/json"
	"fmt"
	"os"
)

type invocation struct {
	Args []string `json:"args"`
}

func main() {
	logPath := os.Getenv("MOCK_VIRTER_LOG")
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mock virter: failed to open log: %v\n", err)
			os.Exit(2)
		}
		json.NewEncoder(f).Encode(invocation{Args: os.Args[1:]})
		f.Close()
	}

	if len(os.Args) < 3 {
		os.Exit(0)
	}

	subcmd := os.Args[1] + " " + os.Args[2]
	failOn := os.Getenv("MOCK_VIRTER_FAIL_ON")
	if failOn != "" && subcmd == failOn {
		fmt.Fprintf(os.Stderr, "mock virter: simulated failure on %q\n", subcmd)
		os.Exit(1)
	}
}
