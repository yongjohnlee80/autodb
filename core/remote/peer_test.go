package remote

import "testing"

// A Peer's sign-in state moves forward once: one device proof (enrolled or
// to enroll), then one sign-in, which binds the session's device.
func TestAPeerProvesOneDeviceAndSignsInOnce(t *testing.T) {
	p := &Peer{}
	if !p.Attest(7) || p.Device() != 7 {
		t.Fatal("the first proof was refused")
	}
	if p.Attest(8) || p.Stage([]byte("k")) || p.Device() != 7 {
		t.Fatal("a second proof after an enrolled device's was taken")
	}
	q := &Peer{}
	if !q.Stage([]byte("k")) || string(q.Pending()) != "k" {
		t.Fatal("staging a device to enroll was refused")
	}
	if q.Attest(9) || q.Stage([]byte("j")) {
		t.Fatal("a second proof after one to enroll was taken")
	}
	if !q.SignIn(3, 11) || q.Session() != 3 || q.Device() != 11 || q.Pending() != nil {
		t.Fatalf("sign-in: session %d device %d pending %q", q.Session(), q.Device(), q.Pending())
	}
	if q.SignIn(4, 11) || q.Session() != 3 {
		t.Fatal("a second sign-in was taken")
	}
	if q.Attest(12) {
		t.Fatal("a proof after sign-in was taken")
	}
}

// OnEnd runs once the connection ends, or at once when it already has.
func TestOnEndRunsOnceAtTheEnd(t *testing.T) {
	p := &Peer{}
	n := 0
	p.OnEnd(func() { n++ })
	if n != 0 {
		t.Fatal("ran before the end")
	}
	p.ended()
	p.ended()
	if n != 1 {
		t.Fatalf("ran %d times, want 1", n)
	}
	p.OnEnd(func() { n++ })
	if n != 2 {
		t.Fatal("registered after the end, it did not run at once")
	}
}
