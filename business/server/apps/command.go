package apps

import (
	"fmt"
	"strings"

	"github.com/kballard/go-shellquote"
)

func parseCommand(command string) ([]string, error) {
	if strings.ContainsRune(command, '\x00') {
		return nil, fmt.Errorf("command cannot contain a null byte")
	}
	words, err := shellquote.Split(command)
	if err != nil {
		return nil, fmt.Errorf("invalid command quoting: %w", err)
	}
	if len(words) == 0 {
		return []string{}, nil
	}
	if words[0] == "" {
		return nil, fmt.Errorf("command must begin with an executable")
	}
	return words, nil
}

func normalizeCommand(command string) (string, error) {
	words, err := parseCommand(command)
	if err != nil {
		return "", err
	}
	return shellquote.Join(words...), nil
}

func renderCommand(command string) ([]string, error) {
	words, err := parseCommand(command)
	if err != nil {
		return nil, err
	}
	for index, word := range words {
		// Kubelet expands $(NAME) even inside shell-quoted arguments. Escape
		// dollars so Borq's parsed words reach the process exactly as written.
		words[index] = strings.ReplaceAll(word, "$", "$$")
	}
	return words, nil
}

func inspectCommand(command, arguments []string) (string, error) {
	if len(command) == 0 && len(arguments) != 0 {
		return "", fmt.Errorf("cannot recover an executable from image arguments alone")
	}
	words := append(append([]string{}, command...), arguments...)
	for index, word := range words {
		var literal strings.Builder
		for offset := 0; offset < len(word); offset++ {
			if word[offset] == '$' && offset+1 < len(word) {
				switch word[offset+1] {
				case '$':
					offset++
				case '(':
					return "", fmt.Errorf("command uses unsupported Kubernetes environment expansion")
				}
			}
			literal.WriteByte(word[offset])
		}
		words[index] = literal.String()
	}
	return normalizeCommand(shellquote.Join(words...))
}
