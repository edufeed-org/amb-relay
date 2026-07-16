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

func validateCalendar(event nostr.Event) (reject bool, msg string) {
	switch event.Kind {
	case 31922, 31923: // date/time calendar events
		if event.Tags.GetD() == "" {
			return true, "missing required 'd' tag"
		}
		if !event.Tags.Has("title") {
			return true, "missing required 'title' tag"
		}
		if !event.Tags.Has("start") {
			return true, "missing required 'start' tag"
		}
	case 31924: // calendar
		if event.Tags.GetD() == "" {
			return true, "missing required 'd' tag"
		}
		if !event.Tags.Has("title") {
			return true, "missing required 'title' tag"
		}
	case 31925: // RSVP
		if event.Tags.GetD() == "" {
			return true, "missing required 'd' tag"
		}
		if !event.Tags.Has("a") {
			return true, "missing required 'a' tag"
		}
		status := ""
		if tag := event.Tags.Find("status"); len(tag) > 1 {
			status = tag[1]
		}
		if status != "accepted" && status != "declined" && status != "tentative" {
			return true, "missing or invalid 'status' tag (accepted/declined/tentative)"
		}
	}
	return false, ""
}

func validateTransferkiosk(event nostr.Event) (reject bool, msg string) {
	if event.Tags.GetD() == "" {
		return true, "missing required 'd' tag"
	}
	if !event.Tags.Has("name") {
		return true, "missing required 'name' tag"
	}
	return false, ""
}

func validatePublication(event nostr.Event) (reject bool, msg string) {
	if event.Tags.GetD() == "" {
		return true, "missing required 'd' tag"
	}
	if !event.Tags.Has("title") {
		return true, "missing required 'title' tag"
	}
	// NKBIP-01: a publication index's content field MUST be empty.
	if event.Kind == 30040 && event.Content != "" {
		return true, "kind 30040 requires empty content (NKBIP-01)"
	}
	return false, ""
}
