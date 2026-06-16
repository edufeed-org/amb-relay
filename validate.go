// validate.go
package main

import "fiatjaf.com/nostr"

func validateAMB(event nostr.Event) (reject bool, msg string) {
	if event.Tags.GetD() == "" {
		return true, "missing required 'd' tag"
	}
	if !event.Tags.Has("name") {
		return true, "missing required 'name' tag"
	}
	return false, ""
}

func validateLongform(event nostr.Event) (reject bool, msg string) {
	if event.Tags.GetD() == "" {
		return true, "missing required 'd' tag"
	}
	if !event.Tags.Has("title") {
		return true, "missing required 'title' tag"
	}
	return false, ""
}

func validateWiki(event nostr.Event) (reject bool, msg string) {
	if event.Tags.GetD() == "" {
		return true, "missing required 'd' tag"
	}
	return false, ""
}
