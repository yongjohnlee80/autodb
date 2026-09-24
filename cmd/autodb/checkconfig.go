package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/yongjohnlee80/autodb/core/config"
)

// VALIDATE BEFORE ANYTHING IS STOPPED.
//
// Observed on a production host upgrading across a release that made
// exec.max_target_conns required: the updater stopped a healthy service,
// installed the new binary, watched it refuse the configuration with
// 78/EX_CONFIG, and rolled back. The front door was down for the whole of that,
// for a reason that was KNOWABLE BEFORE ANYTHING WAS STOPPED -- the new binary
// was already built and on disk.
//
// --check-config turns that rollback into a refusal to start the upgrade at
// all. update_frontdoor.sh runs it after the build and the --version probe and
// before `systemctl stop`.
//
// WHY IT IS ITS OWN FLAG. --create-cert loads the config as a side effect and
// was the nearest thing that existed, but a side effect is not a contract:
// nothing stops it from ceasing to load the config, and it writes key material,
// which is the last thing a pre-flight should do. An operator's pre-flight
// needs to be a mode whose ENTIRE job is the check.
//
// WHAT IT CHECKS, exactly: everything the daemon decides from the configuration
// alone. That is config.Load (path resolution, parse, unknown keys, and every
// semantic constraint), the client-config refusal, and endpoint resolution --
// the three things runServe does before it touches anything. It stops there, on
// purpose, at the first step that is about the HOST rather than the file.
//
// WHAT IT MUST NOT DO: touch anything. It does not bind the port, create the
// socket, open or create the meta store, write key material, or contact a
// running daemon. Running it against a live service must be safe, because it is
// run against one -- while that service is still up. A cell asserts the
// directory is unchanged afterwards, because "it does not touch anything" is a
// claim about behaviour and not about intent.
//
// WHAT IT EXITS WITH: 0 when the configuration would load, and 78 (EX_CONFIG)
// when it would not -- the same code the daemon itself uses, through the same
// reportAndExit path, so a caller branches on one number for both.
func runCheckConfig(w io.Writer, configPath string) error {
	if err := checkConfiguration(w, configPath); err != nil {
		// EVERY REFUSAL THIS MODE CAN PRODUCE IS A CONFIGURATION REFUSAL, and
		// saying so here rather than at each call site closes the class instead
		// of the instance that was found.
		//
		// The instance: requireStoreConfig returns a plain error, so a client
		// config exited 1. The updater refuses ONLY on 78 and reports anything
		// else as "could not check the configuration" -- then stops and swaps a
		// healthy service. A refusal that is read as an inability to refuse is
		// worse than no pre-flight, and every check added to this function
		// later would have inherited the same seam.
		if errors.Is(err, config.ErrInvalid) {
			return err
		}
		return fmt.Errorf("%w: %v", config.ErrInvalid, err)
	}
	return nil
}

func checkConfiguration(w io.Writer, configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	// The same refusal --serve makes, and for the same reason: a client config
	// names no meta store, so serving through one would silently open a
	// private store and leave the real one untouched. A pre-flight that passed
	// here and let the update proceed would be worse than no pre-flight.
	if err := requireStoreConfig(cfg, "serve",
		"Point --config at the daemon's configuration, which is the one the unit passes."); err != nil {
		return err
	}
	ep, err := cfg.Server.Endpoint()
	if err != nil {
		return err
	}

	// NAME WHAT WAS CHECKED. A pre-flight that prints "ok" tells an operator
	// nothing about WHICH file it read -- and reading the wrong one is the
	// failure mode that matters here, because a host can hold a service config
	// and a client config at once.
	fmt.Fprintf(w, "config     : %s\n", describeSource(cfg))
	fmt.Fprintf(w, "endpoint   : %s %s\n", ep.Network, ep.Address)
	fmt.Fprintf(w, "front door : %s\n", frontDoorNote(cfg))
	fmt.Fprintln(w, "the configuration would load; nothing was started and nothing was written")
	return nil
}

// frontDoorNote says whether the surface the update is about is even on, and
// with what budget. The budget is named because it is the setting that has
// actually taken a host down on upgrade.
func frontDoorNote(cfg config.Config) string {
	if !cfg.FrontDoor.Enabled {
		return "disabled"
	}
	return fmt.Sprintf("enabled on %s, exec.max_target_conns = %d",
		cfg.FrontDoor.Bind, cfg.Exec.MaxTargetConns)
}
