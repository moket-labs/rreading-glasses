//go:build linux

package internal

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"testing"

	"github.com/mattn/go-isatty"
)

func TestMaintenanceTerminalDetection(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	if isatty.IsTerminal(read.Fd()) || isatty.IsTerminal(write.Fd()) {
		t.Fatal("pipe classified as terminal")
	}
	null, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if isatty.IsTerminal(null.Fd()) {
		t.Fatal("null device classified as terminal")
	}
	pty, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer pty.Close()
	if !isatty.IsTerminal(pty.Fd()) {
		t.Fatal("actual Linux PTY classified as nonterminal")
	}
}

func TestMaintenanceContainerJSON(t *testing.T) {
	if isatty.IsTerminal(os.Stdout.Fd()) {
		t.Fatal("run this container-log test with non-TTY stdout")
	}
	var output bytes.Buffer
	_logHandler.SetOutput(&output)
	defer _logHandler.SetOutput(os.Stdout)
	slog.Info("maintenance tty probe", "boundary", "container-json")
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("container logger did not emit JSON: %v", err)
	}
	if record["boundary"] != "container-json" || record["msg"] != "maintenance tty probe" {
		t.Fatalf("unexpected JSON record: %v", record)
	}
}
