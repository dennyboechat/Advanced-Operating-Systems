package main

import (
	"fmt"
	"strings"
)

// Pipeline is one parsed input line: one or more commands joined by '|'. The
// standard output of each command becomes the standard input of the next.
type Pipeline struct {
	Cmds       []*Command
	Background bool   // the line ended with '&'
	Text       string // original text, used when listing jobs
}

// Command is one stage of a pipeline: the argument vector handed to exec(),
// and where its standard input and output should point when they are not
// connected to a pipe.
type Command struct {
	Argv    []string // Argv[0] is the program name
	InFile  string   // "< file"
	OutFile string   // "> file" or ">> file"
	Append  bool     // the redirection was ">>"
	Text    string   // this stage's text, e.g. "grep txt"
}

// token is one lexical unit. Operators are only recognized outside quotes, so
// `echo "|"` prints a bar instead of starting a pipeline.
type token struct {
	text string
	op   bool
}

// parse turns a raw input line into a pipeline, or nil for a blank/comment line.
//
//	ls -l | grep txt | sort > out.txt &
//	└─┬─┘   └──┬───┘   └──────┬─────┘ └ Background
//	 Cmds[0]  Cmds[1]       Cmds[2] (with OutFile)
func (sh *Shell) parse(line string) (*Pipeline, error) {
	tokens, err := sh.tokenize(line)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, nil
	}

	p := &Pipeline{}
	cmd := &Command{}
	var words []string // this stage's text, for job listings

	finish := func() error {
		if len(cmd.Argv) == 0 {
			if len(p.Cmds) == 0 && cmd.InFile == "" && cmd.OutFile == "" {
				return fmt.Errorf("syntax error near unexpected token '|'")
			}
			if cmd.InFile != "" || cmd.OutFile != "" {
				return fmt.Errorf("syntax error: redirection without a command")
			}
			return fmt.Errorf("syntax error: missing command after '|'")
		}
		cmd.Text = strings.Join(words, " ")
		p.Cmds = append(p.Cmds, cmd)
		cmd, words = &Command{}, nil
		return nil
	}

	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		if !t.op {
			cmd.Argv = append(cmd.Argv, t.text)
			words = append(words, quoteForDisplay(t.text))
			continue
		}

		switch t.text {
		case "|":
			if err := finish(); err != nil {
				return nil, err
			}
			if i == len(tokens)-1 {
				return nil, fmt.Errorf("syntax error: missing command after '|'")
			}
		case "&":
			if i != len(tokens)-1 {
				return nil, fmt.Errorf("syntax error: '&' must be the last token")
			}
			p.Background = true
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
			words = append(words, t.text, quoteForDisplay(tokens[i].text))
		default:
			return nil, fmt.Errorf("syntax error: unexpected '%s'", t.text)
		}
	}
	if err := finish(); err != nil {
		return nil, err
	}

	stages := make([]string, len(p.Cmds))
	for i, c := range p.Cmds {
		stages[i] = c.Text
	}
	p.Text = strings.Join(stages, " | ")
	return p, nil
}

// quoteForDisplay re-quotes a word that would not survive being typed again.
func quoteForDisplay(w string) string {
	if w == "" || strings.ContainsAny(w, " \t|&<>'\"") {
		return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
	}
	return w
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
			word.WriteString(sh.getenv("HOME"))

		case c == '|':
			flush()
			tokens = append(tokens, token{text: "|", op: true})

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
// exit status of the last command, "$1".."$9" a script's arguments; an unset
// variable expands to the empty string, exactly like a POSIX shell.
func (sh *Shell) expandVar(runes []rune, i int) (string, int) {
	j := i + 1
	if j >= len(runes) {
		return "$", j
	}

	if runes[j] == '?' {
		return fmt.Sprint(sh.lastStatus), j + 1
	}
	if runes[j] == '#' {
		return fmt.Sprint(max(len(sh.args)-1, 0)), j + 1
	}
	if runes[j] >= '0' && runes[j] <= '9' {
		// Positional parameters of a script: $0 is its name, $1 the first argument.
		if n := int(runes[j] - '0'); n < len(sh.args) {
			return sh.args[n], j + 1
		}
		return "", j + 1
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
	return sh.getenv(name), j
}

func isAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}
