// deletion.go — NIP-09 kind-5 deletion requests: write policy + read path.
package main

import (
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
)

// validateDeletion is the write policy for kind-5 deletion requests. The
// relay stores deletions so clients and mirrors can learn that content was
// deleted, but stays scoped to kinds it serves: a deletion whose only
// targets are 'a' coords of foreign kinds is rejected. 'e' targets carry no
// kind, so any deletion with an 'e' tag is accepted.
func validateDeletion(served map[nostr.Kind]bool) func(nostr.Event) (reject bool, msg string) {
	return func(event nostr.Event) (bool, string) {
		hasE, hasA, servesA := false, false, false
		for _, tag := range event.Tags {
			if len(tag) < 2 || tag[1] == "" {
				continue
			}
			switch tag[0] {
			case "e":
				hasE = true
			case "a":
				hasA = true
				if spl := strings.SplitN(tag[1], ":", 2); len(spl) == 2 {
					if n, err := strconv.Atoi(spl[0]); err == nil && served[nostr.Kind(n)] {
						servesA = true
					}
				}
			}
		}
		switch {
		case hasE:
			return false, ""
		case !hasA:
			return true, "missing 'e' or 'a' tag referencing the event to delete"
		case !servesA:
			return true, "deletion references no kind served by this relay"
		}
		return false, ""
	}
}
