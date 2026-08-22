// Package planner builds the deterministic, network-free synchronization plan.
package planner

import (
	"sort"
	"strconv"
	"strings"
	"unicode"

	"sync-release-to-gitee/internal/domain"
)

// BuildPlan applies the Rust-compatible merge, stable sort, tag de-duplication,
// cleanup selection, and GitHub-ID execution order.
func BuildPlan(github, giteeBefore []domain.Release, retainCount int) domain.Plan {
	if retainCount < 0 {
		retainCount = 0
	}

	all := make([]domain.Release, 0, len(giteeBefore)+len(github))
	all = append(all, cloneReleases(giteeBefore)...)
	all = append(all, cloneReleases(github)...)
	sort.SliceStable(all, func(i, j int) bool {
		return CompareTag(all[i].TagName, all[j].TagName) > 0
	})

	seen := make(map[string]struct{}, len(all))
	unique := make([]domain.Release, 0, len(all))
	for _, release := range all {
		if _, found := seen[release.TagName]; found {
			continue
		}
		seen[release.TagName] = struct{}{}
		unique = append(unique, release)
	}

	plan := domain.Plan{}
	retained := make(map[string]struct{})
	for i := 0; i < retainCount && i < len(unique); i++ {
		tag := unique[i].TagName
		plan.RetainedAssetTags = append(plan.RetainedAssetTags, tag)
		retained[tag] = struct{}{}
	}

	existing := make(map[string]struct{}, len(giteeBefore))
	for _, release := range giteeBefore {
		existing[release.TagName] = struct{}{}
		if _, shouldRetain := retained[release.TagName]; !shouldRetain && len(release.Assets) > 2 {
			plan.Cleanup = append(plan.Cleanup, domain.RecreateRelease{
				Release: cloneRelease(release),
				Reason:  "outside retained asset tags with more than two assets",
			})
		}
	}

	syncReleases := cloneReleases(github)
	sort.SliceStable(syncReleases, func(i, j int) bool {
		return syncReleases[i].ID < syncReleases[j].ID
	})
	for _, release := range syncReleases {
		_, exists := existing[release.TagName]
		_, shouldRetain := retained[release.TagName]
		item := domain.SyncRelease{
			Release:        cloneRelease(release),
			RetainAssets:   shouldRetain,
			ExistingAtPlan: exists,
		}
		switch {
		case exists:
			item.Action = domain.ActionSkipExisting
			item.Reason = "matching tag already exists on Gitee"
			plan.SkippedExisting = append(plan.SkippedExisting, release.TagName)
		case shouldRetain && len(release.Assets) > 0:
			item.Action = domain.ActionCreateAndUpload
			item.Reason = "retained tag with GitHub assets"
			plan.CreateAndUpload = append(plan.CreateAndUpload, release.TagName)
		default:
			item.Action = domain.ActionCreateMetadataOnly
			if shouldRetain {
				item.Reason = "retained tag has no GitHub assets"
			} else {
				item.Reason = "tag is outside retained asset tags"
			}
			plan.CreateMetadataOnly = append(plan.CreateMetadataOnly, release.TagName)
		}
		plan.Sync = append(plan.Sync, item)
	}
	return plan
}

// CompareTag mirrors the permissive comparison model from version-compare 0.2.1
// used by the Rust baseline. Tags with no numeric component compare equal.
func CompareTag(left, right string) int {
	leftVersion, leftOK := parseVersion(left)
	rightVersion, rightOK := parseVersion(right)
	if !leftOK || !rightOK {
		return 0
	}
	return compareVersions(leftVersion, rightVersion)
}

type version struct {
	parts []versionPart
}

type versionPart struct {
	number *int32
	text   string
}

func parseVersion(raw string) (version, bool) {
	parts := splitAlphaNumeric(raw)
	parsed := version{}
	hasNumber := false
	for _, part := range parts {
		if value, err := strconv.ParseInt(part, 10, 32); err == nil {
			number := int32(value)
			parsed.parts = append(parsed.parts, versionPart{number: &number})
			hasNumber = true
			continue
		}
		digits := leadingDigits(part)
		if digits > 0 && digits < len(part) {
			if value, err := strconv.ParseInt(part[:digits], 10, 32); err == nil {
				number := int32(value)
				parsed.parts = append(parsed.parts, versionPart{number: &number})
				parsed.parts = append(parsed.parts, versionPart{text: part[digits:]})
				hasNumber = true
				continue
			}
		}
		parsed.parts = append(parsed.parts, versionPart{text: part})
	}
	if len(parsed.parts) == 0 {
		return parsed, true
	}
	return parsed, hasNumber
}

func compareVersions(left, right version) int {
	return comparePartIter(left.parts, right.parts)
}

func comparePartIter(left, right []versionPart) int {
	for index, leftPart := range left {
		if index >= len(right) {
			if leftPart.number != nil {
				if *leftPart.number == 0 {
					continue
				}
				return 1
			}
			return -1
		}
		rightPart := right[index]
		switch {
		case leftPart.number != nil && rightPart.number != nil:
			if *leftPart.number < *rightPart.number {
				return -1
			}
			if *leftPart.number > *rightPart.number {
				return 1
			}
		case leftPart.number == nil && rightPart.number == nil:
			if cmp := strings.Compare(strings.ToLower(leftPart.text), strings.ToLower(rightPart.text)); cmp != 0 {
				return cmp
			}
		}
	}
	if len(right) > len(left) {
		return -comparePartIter(right, left)
	}
	return 0
}

func splitAlphaNumeric(raw string) []string {
	var (
		parts   []string
		current strings.Builder
	)
	flush := func() {
		if current.Len() != 0 {
			parts = append(parts, current.String())
			current.Reset()
		}
	}
	for _, char := range raw {
		if unicode.IsLetter(char) || unicode.IsNumber(char) {
			current.WriteRune(char)
			continue
		}
		flush()
	}
	flush()
	return parts
}

func leadingDigits(value string) int {
	for index, char := range value {
		if char < '0' || char > '9' {
			return index
		}
	}
	return len(value)
}

func cloneRelease(value domain.Release) domain.Release {
	copy := value
	copy.Assets = append([]domain.Asset(nil), value.Assets...)
	return copy
}

func cloneReleases(values []domain.Release) []domain.Release {
	copies := make([]domain.Release, len(values))
	for index, value := range values {
		copies[index] = cloneRelease(value)
	}
	return copies
}
