package tui_test

import (
	"encoding/pem"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/yongjohnlee80/golib/tui/decl/decltest"

	"github.com/yongjohnlee80/autodb/internal/remotetest"
)

// sealKeyFile rewrites the rig's SSH key file with a passphrase of its own.
func (r *remoteRig) sealKeyFile(t *testing.T, pass string) {
	t.Helper()
	raw, err := os.ReadFile(r.srv.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := ssh.ParseRawPrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "test", []byte(pass))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.srv.KeyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A key file with a passphrase: Connect asks for it, naming the file, and
// then connects as usual.
func TestConnectAsksForTheSSHKeysPassphrase(t *testing.T) {
	r := newRemoteRig(t)
	r.sealKeyFile(t, "key-passphrase")
	r.openConnect(t)
	r.answer(t, remotetest.AlicePass, remotetest.AlicePass)
	r.s.WaitFor(t, "the key's passphrase question", func(sc string) bool {
		return strings.Contains(sc, "┌ the SSH key's passphrase ") && strings.Contains(sc, r.srv.KeyFile)
	})
	r.s.Keys(t, append(decltest.Type("key-passphrase"), enter())...)
	r.s.WaitForText(t, "confirm the server's host key")
	r.s.Keys(t, key('t'))
	r.s.WaitFor(t, "signed in on the server", func(string) bool {
		return r.h.Auth() == "signed-in" && strings.Contains(r.h.SourceText("App.status"), "as alice")
	})
}

// Declining the question connects nothing: Connect says why, and the server
// was never reached.
func TestDecliningTheSSHKeysPassphraseConnectsNothing(t *testing.T) {
	r := newRemoteRig(t)
	r.sealKeyFile(t, "key-passphrase")
	denied := r.srv.Count(t, "remote_access_denied")
	r.openConnect(t)
	r.answer(t, remotetest.AlicePass, remotetest.AlicePass)
	r.s.WaitForText(t, "┌ the SSH key's passphrase ")
	r.s.Keys(t, esc())
	r.s.WaitFor(t, "the refusal on Connect", func(sc string) bool {
		return strings.Contains(sc, "the ssh key's passphrase was not given") &&
			strings.Contains(sc, "┌ connect to a remote server ")
	})
	if n := r.srv.Count(t, "remote_access_denied"); n != denied {
		t.Fatalf("remote_access_denied rows %d, want %d: the server was reached", n, denied)
	}
}
