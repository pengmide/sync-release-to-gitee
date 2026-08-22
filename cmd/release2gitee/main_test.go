package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunHelpAndVersion(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--help"}, &stdout, &stderr, mapLookup(nil)); code != 0 {
		t.Fatalf("help exit code = %d", code)
	}
	if !strings.Contains(stdout.String(), "用法：") || !strings.Contains(stdout.String(), "sync-release-to-gitee") || stderr.Len() != 0 {
		t.Fatalf("help stdout/stderr = %q / %q", stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--version"}, &stdout, &stderr, mapLookup(nil)); code != 0 {
		t.Fatalf("version exit code = %d", code)
	}
	if strings.TrimSpace(stdout.String()) != version || stderr.Len() != 0 {
		t.Fatalf("version stdout/stderr = %q / %q", stdout.String(), stderr.String())
	}
}

func TestRunMissingRequiredConfigDoesNotLeakToken(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run([]string{"--gitee-token=very-secret"}, &stdout, &stderr, mapLookup(nil))
	if code != 2 {
		t.Fatalf("missing config exit code = %d", code)
	}
	if strings.Contains(stderr.String(), "very-secret") {
		t.Fatalf("stderr leaked token: %q", stderr.String())
	}
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
