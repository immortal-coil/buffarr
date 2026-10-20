package identify

// UnidentifiedKey is the CDN-level fallback "game identifier" for content
// that can't be resolved to an individual game (R3): all unmapped content
// for a platform intentionally shares this one bucket rather than being
// individually distinguished -- e.g. "epic:unidentified". This is the
// nature of the R3 fallback, not a defect.
func UnidentifiedKey(platform string) string {
	return platform + ":unidentified"
}
