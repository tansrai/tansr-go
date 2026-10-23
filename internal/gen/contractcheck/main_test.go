package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"success", []string{"-contract", filepath.Join("..", "..", "..", "contract")}, 0, "verified: revision=17"},
		{"missing directory", []string{"-contract", filepath.Join(t.TempDir(), "missing")}, 1, "open contract directory"},
		{"help", []string{"-h"}, 0, "optional tansr-cli checkout"},
		{"unknown flag", []string{"-update"}, 2, "flag provided but not defined"},
		{"positional argument", []string{"unexpected"}, 2, "unexpected positional argument"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			output := stdout.String() + stderr.String()
			if code != tt.code || !strings.Contains(output, tt.want) {
				t.Fatalf("code=%d, output=%q; want code=%d containing %q", code, output, tt.code, tt.want)
			}
		})
	}
}
