package main

import (
	"fmt"
	"os"
	"strings"
)

// Command is one parsed command line: the argument vector handed to exec(),
// where its three standard file descriptors should point, and whether the
// shell should wait for it.
type Command struct {
	Argv       []string // Argv[0] is the program name
	Background bool     // the line ended with '&'
	InFile     string   // "< file", empty when inherited from the shell
	OutFile    string   // "> file" or ">> file"
	Append     bool     // the redirection was ">>"
	Text       string   // original text, used when listing jobs
}

// token is one lexical unit. Operators are only recognized outside quotes, so
// `echo ">"` prints a greater-than sign instead of redirecting.
type token struct {
	text string
	op   bool
}

// parse turns a raw input line into a command, or nil for a blank/comment line.
func (sh *Shell) parse(line string) (*Command, error) {
	tokens, err := sh.tokenize(line)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, nil
	}

	cmd := &Command{Text: strings.TrimSpace(line)}
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		if !t.op {
			cmd.Argv = append(cmd.Argv, t.text)
			continue
		}

		switch t.text {
		case "&":
			if i != len(tokens)-1 {
				return nil, fmt.Errorf("syntax error: '&' must be the last token")
			}
			cmd.Background = true
			cmd.Text = strings.TrimSpace(strings.TrimSuffix(cmd.Text, "&"))
		case "<", ">", ">>":
			if i+1 >= len(tokens) || tokens[i+1].op {
				return nil, fmt.Errorf("syntax error: missing file name after '%s'", t.text)
			}
			i++
			if t.text == "<" {
				cmd.InFile = tokens[i].text
			} else {
				cmd.OutFile = tokens[i].text
				cmd.Append = t.text == ">>"
			}
		default:
			return nil, fmt.Errorf("syntax error: unexpected '%s'", t.text)
		}
	}

	if len(cmd.Argv) == 0 {
		return nil, fmt.Errorf("syntax error: redirection without a command")
	}
	return cmd, nil
}

// tokenize splits a line into words and operators, honouring single quotes
// (fully literal), double quotes (backslash escapes and variables) and
// backslash escapes. Variable and tilde expansion happen here, so that a
// quoted '$HOME' survives untouched.
func (sh *Shell) tokenize(line string) ([]token, error) {
	var (
		tokens  []token
		word    strings.Builder
		started bool // a word is being built (may legitimately be empty: "")
	)

	flush := func() {
		if started {
			tokens = append(tokens, token{text: word.String()})
			word.Reset()
			started = false
		}
	}

	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == '#' && !started:
			// Comment: ignore the rest of the line.
			i = len(runes)

		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()

		case c == '\'':
			started = true
			j := i + 1
			for j < len(runes) && runes[j] != '\'' {
				word.WriteRune(runes[j])
				j++
			}
			if j == len(runes) {
				return nil, fmt.Errorf("syntax error: unterminated single quote")
			}
			i = j

		case c == '"':
			started = true
			j := i + 1
			for j < len(runes) && runes[j] != '"' {
				switch {
				case runes[j] == '\\' && j+1 < len(runes) && strings.ContainsRune(`"\$`, runes[j+1]):
					j++
					word.WriteRune(runes[j])
				case runes[j] == '$':
					value, next := sh.expandVar(runes, j)
					word.WriteString(value)
					j = next - 1
				default:
					word.WriteRune(runes[j])
				}
				j++
			}
			if j == len(runes) {
				return nil, fmt.Errorf("syntax error: unterminated double quote")
			}
			i = j

		case c == '\\':
			started = true
			if i+1 < len(runes) {
				i++
				word.WriteRune(runes[i])
			}

		case c == '$':
			started = true
			value, next := sh.expandVar(runes, i)
			word.WriteString(value)
			i = next - 1

		case c == '~' && !started && (i+1 == len(runes) || runes[i+1] == '/' || runes[i+1] == ' '):
			started = true
			word.WriteString(homeDir())

		case c == '&':
			flush()
			tokens = append(tokens, token{text: "&", op: true})

		case c == '<':
			flush()
			tokens = append(tokens, token{text: "<", op: true})

		case c == '>':
			flush()
			if i+1 < len(runes) && runes[i+1] == '>' {
				i++
				tokens = append(tokens, token{text: ">>", op: true})
			} else {
				tokens = append(tokens, token{text: ">", op: true})
			}

		default:
			started = true
			word.WriteRune(c)
		}
	}
	flush()
	return tokens, nil
}

// expandVar reads the variable reference starting at runes[i] (which is '$')
// and returns its value plus the index just past the reference. "$?" yields the
// exit status of the last command; an unset variable expands to the empty
// string, exactly like a POSIX shell.
func (sh *Shell) expandVar(runes []rune, i int) (string, int) {
	j := i + 1
	if j >= len(runes) {
		return "$", j
	}

	if runes[j] == '?' {
		return fmt.Sprint(sh.lastStatus), j + 1
	}

	braced := runes[j] == '{'
	if braced {
		j++
	}

	start := j
	for j < len(runes) && (runes[j] == '_' || isAlnum(runes[j])) {
		j++
	}
	name := string(runes[start:j])

	if braced {
		if j >= len(runes) || runes[j] != '}' {
			return "$" + name, j // malformed, leave it alone
		}
		j++
	}
	if name == "" {
		return "$", i + 1
	}
	return os.Getenv(name), j
}

func isAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}
