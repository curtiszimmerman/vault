package token

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kardianos/osext"
)

var exePath string

func init() {
	var err error
	exePath, err = osext.Executable()
	if err != nil {
		panic("failed to detect self path: " + err.Error())
	}
}

// HelperPath takes the configured path to a helper and expands it to
// a full absolute path that can be executed. If the path is relative then
// a prefix of "vault token-" will be prepended to the path.
func HelperPath(path string) string {
	space := strings.Index(path, " ")
	if space == -1 {
		space = len(path)
	}

	// Get the binary name. If it isn't absolute, prepend "vault token-"
	binary := path[0:space]
	if !filepath.IsAbs(binary) {
		binary = exePath + " token-" + binary
	}

	// Return the resulting string
	return fmt.Sprintf("%s%s", binary, path[space:])
}

// Helper is the struct that has all the logic for storing and retrieving
// tokens from the token helper. The API for the helpers is simple: the
// Path is executed within a shell with environment Env. The last argument
// appended will be the operation, which is:
//
//   * "get" - Read the value of the token and write it to stdout.
//   * "store" - Store the value of the token which is on stdin. Output
//       nothing.
//   * "erase" - Erase the contents stored. Output nothing.
//
// Any errors can be written on stdout. If the helper exits with a non-zero
// exit code then the stderr will be made part of the error value.
type Helper struct {
	Path string
	Env  []string
}

// Erase deletes the contents from the helper.
func (h *Helper) Erase() error {
	cmd, err := h.cmd("erase")
	if err != nil {
		return fmt.Errorf("Error: %s", err)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf(
			"Error: %s\n\n%s", err, string(output))
	}
	return nil
}

// Get gets the token value from the helper.
func (h *Helper) Get() (string, error) {
	var buf, stderr bytes.Buffer
	cmd, err := h.cmd("get")
	if err != nil {
		return "", fmt.Errorf("Error: %s", err)
	}
	cmd.Stdout = &buf
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf(
			"Error: %s\n\n%s", err, stderr.String())
	}

	return buf.String(), nil
}

// Store stores the token value into the helper.
func (h *Helper) Store(v string) error {
	buf := bytes.NewBufferString(v)
	cmd, err := h.cmd("store")
	if err != nil {
		return fmt.Errorf("Error: %s", err)
	}
	cmd.Stdin = buf
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf(
			"Error: %s\n\n%s", err, string(output))
	}

	return nil
}

func (h *Helper) cmd(op string) (*exec.Cmd, error) {
	script := strings.Replace(h.Path, "\\", "\\\\", -1) + " " + op
	cmd, err := ExecScript(script)
	if err != nil {
		return nil, err
	}
	cmd.Env = h.Env
	return cmd, nil
}

// ExecScript returns a command to execute a script
func ExecScript(script string) (*exec.Cmd, error) {
	var shell, flag string
	if runtime.GOOS == "windows" {
		shell = "cmd"
		flag = "/C"
	} else {
		shell = "/bin/sh"
		flag = "-c"
	}
	if other := os.Getenv("SHELL"); other != "" {
		shell = other
	}
	cmd := exec.Command(shell, flag, script)
	return cmd, nil
}
