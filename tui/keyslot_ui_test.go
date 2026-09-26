package tui_test

import (
	"strings"
	"testing"
	"time"

	tuiapp "github.com/yongjohnlee80/autodb/tui"
)

func TestKeyslotConsentKeepsItsProseAndRefreshesAfterEnrollment(t *testing.T) {
	h, s := signedIn(t)
	enroll, remove := make(chan struct{}, 1), make(chan struct{}, 1)
	h.FakeKeyslot(tuiapp.KeyslotStatus{StoreUnlocked: true, SlotPresentKnown: true}, enroll, remove)
	s.Keys(t, key(' '), key('K'))
	s.WaitForText(t, "UNATTENDED UNLOCK: NOT ENABLED")
	if !h.SourceBool("App.keyslotCanEnable") {
		t.Fatal("unlocked store hid enrollment")
	}
	s.Keys(t, key('e'))
	s.WaitForText(t, "┌ enable unattended unlock? ")
	if text := h.SourceText("App.keyslotQuestion"); !strings.Contains(text, "Anyone who can read BOTH") || !strings.Contains(text, "0600") {
		t.Fatal("the enrollment consent omits the keyfile/meta-store consequence")
	}
	s.Keys(t, enter())
	if !strings.Contains(s.String(), "┌ enable unattended unlock? ") {
		t.Fatal("bare Enter enrolled a service keyslot")
	}
	s.Keys(t, tab(), key(' '))
	select {
	case <-enroll:
	case <-time.After(3 * time.Second):
		t.Fatal("confirmed enrollment did not reach the pinned action")
	}
	s.WaitForText(t, "UNATTENDED UNLOCK: ENROLLED AND VERIFIED")
	if h.SourceBool("App.keyslotCanEnable") || !h.SourceBool("App.keyslotCanRemove") {
		t.Fatal("keyslot buttons did not follow the verified state")
	}
	select {
	case <-remove:
		t.Fatal("enroll invoked removal too")
	default:
	}
	s.Keys(t, key('r'))
	s.WaitForText(t, "┌ remove service keyslot? ")
	if !strings.Contains(h.SourceText("App.keyslotQuestion"), "next restart this daemon is LOCKED") {
		t.Fatal("removal consent omitted the post-restart outage")
	}
	s.Keys(t, esc())
	s.WaitForText(t, "┌ service keyslot ")
	s.Keys(t, key('r'))
	s.WaitForText(t, "┌ remove service keyslot? ")
	s.Keys(t, tab(), key(' '))
	select {
	case <-remove:
	case <-time.After(3 * time.Second):
		t.Fatal("confirmed removal did not reach its pinned action")
	}
	s.WaitForText(t, "UNATTENDED UNLOCK: REMOVED")
}
