package remoteclient

import (
	"bytes"
	"sync"
	"testing"

	"crypto/ed25519"
)

// A held device key is copied out whole and wiped in place: the copy is
// private (the holder's bytes are not the caller's), Wipe zeroes them and
// forgets the key, and a new key replaces an old one without leaving its bytes.
func TestADeviceKeyIsCopiedHeldAndWiped(t *testing.T) {
	priv, err := NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	var d DeviceKey
	if got := d.Copy(); got != nil {
		t.Fatal("the zero holder copied a key")
	}
	d.Adopt(priv)
	got := d.Copy()
	if !bytes.Equal(got, priv) {
		t.Fatalf("Copy returned a different key")
	}
	// the copy is private: writing it does not touch the holder's bytes
	got[0] ^= 0xff
	if d.key[0] == got[0] {
		t.Fatal("the holder's key changed with the copy's")
	}
	held := d.key // the very slice the holder holds, to see Wipe zero it
	d.Wipe()
	if got := d.Copy(); got != nil {
		t.Fatal("a wiped holder still copied a key")
	}
	if len(d.key) != 0 {
		t.Fatal("Wipe did not forget the key")
	}
	for _, b := range held {
		if b != 0 {
			t.Fatal("Wipe left key bytes in memory")
		}
	}
}

// Adopting a second key wipes the first: a rotation leaves one key in memory.
func TestAdoptingASecondKeyWipesTheFirst(t *testing.T) {
	first, err := NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	var d DeviceKey
	d.Adopt(first)
	d.Adopt(second)
	if !bytes.Equal(d.key, second) {
		t.Fatal("Adopt did not install the second key")
	}
	for _, b := range first { // the very slice the holder held
		if b != 0 {
			t.Fatal("the first key's bytes survived the adoption")
		}
	}
}

// A disconnect wipes the held key while a reconnect's dial copies it, on the
// TUI's workers, and the two must not cross: this hammer runs them against
// each other, and the race detector says whether they did. Each goroutine
// owns the key it adopts, so the holder's wipes stay behind its lock.
func TestTheDeviceKeyCopiesWhileItIsWiped(t *testing.T) {
	priv, err := NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	var d DeviceKey
	d.Adopt(priv)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { // the reconnect dials
			defer wg.Done()
			for range 200 {
				if k := d.Copy(); k != nil && len(k) != ed25519.PrivateKeySize {
					panic("a copied key is torn")
				}
			}
		}()
		own, err := NewDeviceKey()
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() { // the disconnects and the reconnects' adoptions
			defer wg.Done()
			for range 200 {
				d.Wipe()
				d.Adopt(own)
			}
		}()
	}
	wg.Wait()
	d.Wipe()
}
