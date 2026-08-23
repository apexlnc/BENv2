package main

import (
	"os"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/agenttest"
	"github.com/srhg-ai-7cef3f93/ben/internal/agent/claudecode/testdata/fakesrt"
)

func main() {
	agenttest.DumpInvocation("")
	fakesrt.Run(os.Args[1:])
}
