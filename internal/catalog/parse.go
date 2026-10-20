package catalog

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// logLineRE matches docker/nginx/conf.d/05_log_format.conf's "cachelog"
// format exactly:
//
//	[$cacheidentifier] $remote_addr - $remote_user [$time_local] "$request" $status $body_bytes_sent "$http_referer" "$http_user_agent" "$upstream_cache_status" "$host"
var logLineRE = regexp.MustCompile(
	`^\[([^\]]*)\] (\S+) - (\S+) \[([^\]]+)\] "([A-Z]+) (\S+)[^"]*" (\d+) (\d+) "[^"]*" "[^"]*" "([^"]*)" "([^"]*)"$`,
)

const nginxTimeLocalLayout = "02/Jan/2006:15:04:05 -0700"

// LogLine is one parsed nginx access log entry.
type LogLine struct {
	CacheIdentifier     string
	Method              string
	URI                 string
	Status              int
	BodyBytesSent       int64
	UpstreamCacheStatus string
	Host                string
	Time                time.Time
}

// ParseLine parses one cachelog-format line. A line that doesn't match the
// expected format is reported as an error rather than silently dropped, so
// a log-format mismatch (e.g. a config drift between 05_log_format.conf
// and this parser) surfaces instead of quietly losing rows.
func ParseLine(line string) (LogLine, error) {
	m := logLineRE.FindStringSubmatch(line)
	if m == nil {
		return LogLine{}, fmt.Errorf("line does not match the cachelog format: %q", line)
	}

	status, err := strconv.Atoi(m[7])
	if err != nil {
		return LogLine{}, fmt.Errorf("parsing status %q: %w", m[7], err)
	}
	size, err := strconv.ParseInt(m[8], 10, 64)
	if err != nil {
		return LogLine{}, fmt.Errorf("parsing body_bytes_sent %q: %w", m[8], err)
	}
	t, err := time.Parse(nginxTimeLocalLayout, m[4])
	if err != nil {
		return LogLine{}, fmt.Errorf("parsing time_local %q: %w", m[4], err)
	}

	return LogLine{
		CacheIdentifier:     m[1],
		Method:              m[5],
		URI:                 m[6],
		Status:              status,
		BodyBytesSent:       size,
		UpstreamCacheStatus: m[9],
		Host:                m[10],
		Time:                t,
	}, nil
}

// CacheKey is the catalog's per-entry key: $cacheidentifier + URI, the
// prefix of nginx's real on-disk proxy_cache_key
// ($cacheidentifier$uri$slice_range) before the per-slice $slice_range
// suffix. The catalog intentionally tracks at this granularity (one row
// per downloadable object, e.g. one manifest or chunk file) rather than
// per nginx slice -- R2's "disk space consumed" is per game, and KTD2's
// slicing is an nginx-internal mechanism, not a catalog-visible one.
// reconcile.go strips slice_range back off the on-disk KEY: line to
// correlate disk files to this same string.
func CacheKey(line LogLine) string {
	return line.CacheIdentifier + line.URI
}
