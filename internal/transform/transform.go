// Package transform contains the intentionally narrow legacy URL rewrites.
package transform

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Options identifies the source and target repositories used in URL rewrites.
type Options struct {
	GitHubOwner string
	GitHubRepo  string
	GiteeOwner  string
	GiteeRepo   string
}

// ReplaceRepoURL applies the same exact string replacement as the Rust client.
func ReplaceRepoURL(content string, options Options) string {
	source := fmt.Sprintf("https://github.com/%s/%s", options.GitHubOwner, options.GitHubRepo)
	target := fmt.Sprintf("https://gitee.com/%s/%s", options.GiteeOwner, options.GiteeRepo)
	return strings.ReplaceAll(content, source, target)
}

// GitHubBody normalizes GitHub's null/empty body to the tag before optionally
// applying the legacy repository URL replacement.
func GitHubBody(body *string, tag string, enabled bool, options Options) string {
	value := ""
	if body != nil {
		value = *body
	}
	if value == "" {
		value = tag
	}
	if !enabled {
		return value
	}
	return ReplaceRepoURL(value, options)
}

// ExistingBody rewrites an already-existing Gitee body for delete/recreate.
// Unlike GitHubBody, a missing value remains an empty string.
func ExistingBody(body *string, enabled bool, options Options) string {
	value := ""
	if body != nil {
		value = *body
	}
	if !enabled {
		return value
	}
	return ReplaceRepoURL(value, options)
}

// LatestJSON transforms only valid UTF-8 payloads. This preserves the legacy
// behavior where a non-text latest.json fails before any remote write.
func LatestJSON(content []byte, enabled bool, options Options) ([]byte, error) {
	if !enabled {
		return append([]byte(nil), content...), nil
	}
	if !utf8.Valid(content) {
		return nil, fmt.Errorf("latest.json is not valid UTF-8")
	}
	return []byte(ReplaceRepoURL(string(content), options)), nil
}
