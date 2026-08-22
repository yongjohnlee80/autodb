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
		port                            int
		portSet                         bool
	}
	ok := map[string]args{
		"serve alone":          {serve: true, port: goodPort},
		"ui alone":             {ui: true, port: goodPort},
		"web-ui alone":         {webUI: true, port: goodPort},
		"print-endpoint alone": {printEndpoint: true, port: goodPort},
		"web-ui with a port":   {webUI: true, port: 9999, portSet: true},
		"no mode (usage)":      {port: goodPort},
	}
	bad := map[string]args{
		"web-ui + print-endpoint": {webUI: true, printEndpoint: true, port: goodPort},
		"web-ui + serve":          {webUI: true, serve: true, port: goodPort},
		"web-ui + ui":             {webUI: true, ui: true, port: goodPort},
		"serve + ui":              {serve: true, ui: true, port: goodPort},
		"serve + print-endpoint":  {serve: true, printEndpoint: true, port: goodPort},
		"three at once":           {serve: true, ui: true, webUI: true, port: goodPort},
		"port without web-ui":     {ui: true, port: goodPort, portSet: true},
		"web-ui port 0":           {webUI: true, port: 0, portSet: true},
		"web-ui port too high":    {webUI: true, port: 70000, portSet: true},
		"web-ui negative port":    {webUI: true, port: -1, portSet: true},
	}

	run := func(a args) error {
		// checkFlags reads --port's PRESENCE from flag.CommandLine, so a fresh
		// FlagSet is set up per case to reflect portSet.
		reset(t, a.portSet)
		return checkFlags(a.serve, a.ui, a.webUI, a.printEndpoint, a.port)
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
func reset(t *testing.T, portSet bool) {
	t.Helper()
	fs := flag.NewFlagSet("autodb", flag.ContinueOnError)
	fs.Int("port", 7010, "")
	args := []string{}
	if portSet {
		args = []string{"--port=7010"}
	}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	flag.CommandLine = fs
}
