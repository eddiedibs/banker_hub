package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// ANSI styles. Empty strings when color is off.
type palette struct{ bold, dim, green, yellow, red, cyan, reset string }

func newPalette(on bool) palette {
	if !on {
		return palette{}
	}
	return palette{
		bold: "\033[1m", dim: "\033[2m", green: "\033[32m", yellow: "\033[33m",
		red: "\033[31m", cyan: "\033[36m", reset: "\033[0m",
	}
}

// prompter asks questions on a line-based input.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
	c   palette
	// readSecret reads one line without echo. Nil means read a normal line.
	readSecret func() (string, error)
}

// newTerminalPrompter uses stdin/stdout. Color and hidden input are
// enabled only when stdin and stdout are terminals.
func newTerminalPrompter() *prompter {
	p := &prompter{in: bufio.NewReader(os.Stdin), out: os.Stdout}
	inTTY := term.IsTerminal(int(os.Stdin.Fd()))
	outTTY := term.IsTerminal(int(os.Stdout.Fd()))
	p.c = newPalette(outTTY && os.Getenv("NO_COLOR") == "")
	if inTTY {
		p.readSecret = func() (string, error) {
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(p.out)
			return string(b), err
		}
	}
	return p
}

// errAborted is returned when the input ends before the questions do.
var errAborted = errors.New("input closed, setup aborted")

func (p *prompter) line() (string, error) {
	s, err := p.in.ReadString('\n')
	if err != nil && (s == "" || !errors.Is(err, io.EOF)) {
		if errors.Is(err, io.EOF) {
			return "", errAborted
		}
		return "", err
	}
	return strings.TrimSpace(s), nil
}

// ask prints label with an optional default and repeats until validate passes.
func (p *prompter) ask(label, def string, validate func(string) error) (string, error) {
	for {
		if def != "" {
			fmt.Fprintf(p.out, "  %s %s[%s]%s: ", label, p.c.dim, def, p.c.reset)
		} else {
			fmt.Fprintf(p.out, "  %s: ", label)
		}
		v, err := p.line()
		if err != nil {
			return "", err
		}
		if v == "" {
			v = def
		}
		if validate != nil {
			if err := validate(v); err != nil {
				p.fail(err.Error())
				continue
			}
		}
		return v, nil
	}
}

// secret reads a value without echo. With an existing value, an empty
// answer keeps it.
func (p *prompter) secret(label, existing string) (string, error) {
	for {
		hint := ""
		if existing != "" {
			hint = fmt.Sprintf(" %s[saved, Enter to keep]%s", p.c.dim, p.c.reset)
		}
		fmt.Fprintf(p.out, "  %s%s: ", label, hint)
		var v string
		var err error
		if p.readSecret != nil {
			v, err = p.readSecret()
			v = strings.TrimSpace(v)
		} else {
			v, err = p.line()
		}
		if err != nil {
			return "", err
		}
		if v == "" {
			v = existing
		}
		if v != "" {
			return v, nil
		}
		p.fail("value is required")
	}
}

// confirm asks a yes/no question.
func (p *prompter) confirm(label string, def bool) (bool, error) {
	opts := "y/N"
	if def {
		opts = "Y/n"
	}
	for {
		fmt.Fprintf(p.out, "  %s %s[%s]%s: ", label, p.c.dim, opts, p.c.reset)
		v, err := p.line()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(v) {
		case "":
			return def, nil
		case "y", "yes", "s", "si", "sí":
			return true, nil
		case "n", "no":
			return false, nil
		}
		p.fail("answer y or n")
	}
}

func (p *prompter) step(n, total int, title string) {
	fmt.Fprintf(p.out, "\n%s%s[%d/%d] %s%s\n", p.c.bold, p.c.cyan, n, total, title, p.c.reset)
}

func (p *prompter) ok(msg string)   { fmt.Fprintf(p.out, "  %s✓%s %s\n", p.c.green, p.c.reset, msg) }
func (p *prompter) warn(msg string) { fmt.Fprintf(p.out, "  %s!%s %s\n", p.c.yellow, p.c.reset, msg) }
func (p *prompter) fail(msg string) { fmt.Fprintf(p.out, "  %s✗%s %s\n", p.c.red, p.c.reset, msg) }
func (p *prompter) note(msg string) { fmt.Fprintf(p.out, "  %s%s%s\n", p.c.dim, msg, p.c.reset) }
