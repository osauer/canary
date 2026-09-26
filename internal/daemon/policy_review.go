package daemon

import (
	"bufio"
	"bytes"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

// PolicyUnreviewedMarker is the header line Canary writes at the top of every
// policy file it materializes from its own defaults. While the line is in the
// file's leading comment block the values are Canary's, not the owner's, and
// every surface reports the file as "default, unreviewed". The owner removes
// the line once they have reviewed the file; nothing else reads it.
const PolicyUnreviewedMarker = "# Canary defaults, not yet reviewed."

// policyFileReview reports rpc.PolicyReviewUnreviewed when the marker sits in
// the file's leading comment block, and "" otherwise. A marker further down,
// after the first key, is ordinary text and does not count.
func policyFileReview(data []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == PolicyUnreviewedMarker:
			return rpc.PolicyReviewUnreviewed
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		default:
			return ""
		}
	}
	return ""
}
