package main

import (
	"flag"
	"testing"
)

// Exactly one dispatch mode. The switch tries print-endpoint, serve, ui, web-ui in
// order, so any pairing silently runs whichever comes first — which is how
// `--web-ui --print-endpoint` printed the endpoint and never served the UI (lector
// r3 must-fix 2). Every pairwise conflict is checked, print-endpoint included.
func TestCheckFlags(t *testing.T) {
	t.Parallel()
	const goodPort = 7010

	type args struct {
		serve, ui, webUI, printEndpoint bool
		migrateToPG                     bool
		createCert                      bool
		port                            int
		portSet                         bool
		// certFlags are --create-cert's own flags, given by NAME so the cell
		// exercises the same presence check checkFlags reads.
		certFlags []string
	}
	ok := map[string]args{
		"serve alone":          {serve: true, port: goodPort},
		"migrate alone":        {migrateToPG: true, port: goodPort},
		"ui alone":             {ui: true, port: goodPort},
		"web-ui alone":         {webUI: true, port: goodPort},
		"print-endpoint alone": {printEndpoint: true, port: goodPort},
		"web-ui with a port":   {webUI: true, port: 9999, portSet: true},
		"no mode (usage)":      {port: goodPort},
		"create-cert alone":    {createCert: true, port: goodPort},
		"create-cert with its own flags": {createCert: true, port: goodPort,
			certFlags: []string{"--cert-dir=/tmp/x", "--leaf-only", "--force"}},
		"create-cert export": {createCert: true, port: goodPort,
			certFlags: []string{"--export-ca"}},
	}
	bad := map[string]args{
		"web-ui + print-endpoint": {webUI: true, printEndpoint: true, port: goodPort},
		"web-ui + serve":          {webUI: true, serve: true, port: goodPort},
		"web-ui + ui":             {webUI: true, ui: true, port: goodPort},
		"serve + ui":              {serve: true, ui: true, port: goodPort},
		"serve + print-endpoint":  {serve: true, printEndpoint: true, port: goodPort},
		"ui + print-endpoint":     {ui: true, printEndpoint: true, port: goodPort},
		"three at once":           {serve: true, ui: true, webUI: true, port: goodPort},
		// --migrate-to-postgres is FIRST in the dispatch switch, so an
		// uncounted pairing would migrate and never serve — the same class of
		// bug as the web-ui/print-endpoint pairing this table was built for.
		"migrate + serve":      {migrateToPG: true, serve: true, port: goodPort},
		"migrate + ui":         {migrateToPG: true, ui: true, port: goodPort},
		"port without web-ui":  {ui: true, port: goodPort, portSet: true},
		"web-ui port 0":        {webUI: true, port: 0, portSet: true},
		"web-ui port too high": {webUI: true, port: 70000, portSet: true},
		"web-ui negative port": {webUI: true, port: -1, portSet: true},
		// --create-cert is FIRST in the dispatch switch now, so an uncounted
		// pairing would generate a certificate and never serve.
		"create-cert + serve":   {createCert: true, serve: true, port: goodPort},
		"create-cert + ui":      {createCert: true, ui: true, port: goodPort},
		"create-cert + migrate": {createCert: true, migrateToPG: true, port: goodPort},
		// A cert flag outside its mode. --force is the one that matters:
		// "do it anyway" is the last impression to leave on a flag that can
		// invalidate every ca.pem an organisation has distributed.
		"force without create-cert":    {serve: true, port: goodPort, certFlags: []string{"--force"}},
		"cert-dir without create-cert": {serve: true, port: goodPort, certFlags: []string{"--cert-dir=/tmp/x"}},
		"cert-hosts without create-cert": {ui: true, port: goodPort,
			certFlags: []string{"--cert-hosts=a.example.com"}},
		// Two different intentions; the dispatch would honour whichever is
		// checked first and silently drop the other.
		"leaf-only + export-ca": {createCert: true, port: goodPort,
			certFlags: []string{"--leaf-only", "--export-ca"}},
	}

	run := func(a args) error {
		// checkFlags reads --port's PRESENCE from flag.CommandLine, so a fresh
		// FlagSet is set up per case to reflect portSet.
		reset(t, a.portSet, a.certFlags...)
		return checkFlags(a.serve, a.ui, a.webUI, a.printEndpoint, a.migrateToPG, a.createCert, a.port)
	}
	for name, a := range ok {
		t.Run("ok/"+name, func(t *testing.T) {
			if err := run(a); err != nil {
				t.Errorf("rejected a valid combination: %v", err)
			}
		})
	}
	for name, a := range bad {
		t.Run("bad/"+name, func(t *testing.T) {
			if err := run(a); err == nil {
				t.Error("accepted a combination that cannot mean anything")
			}
		})
	}
}

// reset installs a fresh flag.CommandLine, optionally with --port marked as
// visited, so checkFlags's presence probe reflects the case.
func reset(t *testing.T, portSet bool, extra ...string) {
	t.Helper()
	fs := flag.NewFlagSet("autodb", flag.ContinueOnError)
	fs.Int("port", 7010, "")
	fs.String("cert-dir", "", "")
	var hosts hostList
	fs.Var(&hosts, "cert-hosts", "")
	fs.Bool("leaf-only", false, "")
	fs.Bool("export-ca", false, "")
	fs.Bool("force", false, "")
	args := append([]string{}, extra...)
	if portSet {
		args = append(args, "--port=7010")
	}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	flag.CommandLine = fs
}
