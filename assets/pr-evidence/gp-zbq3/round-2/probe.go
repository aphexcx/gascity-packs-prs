// Run only in a scratch module containing copies of the production redactor.
package main

import (
	"encoding/json"
	"github.com/sjarmak/gc-slack-adapter/redact"
	"os"
)

func main() {
	var texts []string
	if err := json.NewDecoder(os.Stdin).Decode(&texts); err != nil {
		panic(err)
	}
	outputs := make([]string, len(texts))
	for i, text := range texts {
		outputs[i] = redact.Text(text)
	}
	if err := json.NewEncoder(os.Stdout).Encode(outputs); err != nil {
		panic(err)
	}
}
