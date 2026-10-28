package update

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// Options are what `opiz3-router -update` and `-rollback` were asked to do.
type Options struct {
	// Current is the running version, as stamped into the binary.
	Current string
	Repo    string
	// APIBase is GitHub's API URL, for tests; empty means GitHub.
	APIBase string
	// Version installs this release instead of the latest, which can go back.
	Version string
	// Check only reports whether a newer release exists.
	Check bool
	// Force installs even when already up to date or running a build that has
	// no release version.
	Force bool
	// Rollback goes back to the version before the last update.
	Rollback  bool
	NoRestart bool
	// Service is restarted afterwards.
	Service string
	// Exe is the binary to replace; the running one when empty.
	Exe string
	Out io.Writer
}

// Run does what Options asks.
func Run(ctx context.Context, o Options) error {
	say := func(format string, args ...any) { fmt.Fprintf(o.Out, format+"\n", args...) }
	exe, e := o.Exe, error(nil)
	if exe == "" {
		if exe, e = Executable(); e != nil {
			return e
		}
	}

	if o.Rollback {
		if e = Rollback(exe); e != nil {
			return e
		}
		say("Went back to the previous version.")
		return o.restart(say)
	}

	c := New(o.Repo, o.APIBase)
	var rel *Release
	if o.Version != "" {
		rel, e = c.ByTag(ctx, o.Version)
	} else {
		rel, e = c.Latest(ctx)
	}
	if e != nil {
		return e
	}

	if o.Version == "" {
		newer, cmpErr := Newer(o.Current, rel.Tag)
		switch {
		case cmpErr != nil && !o.Force:
			if o.Check {
				say("The latest release is %s. This build (%s) has no release version, so it can't tell if that is newer.", rel.Tag, o.Current)
				return nil
			}
			return fmt.Errorf("this build (%s) has no release version, so it can't tell if %s is newer; use -force to install it anyway", o.Current, rel.Tag)
		case cmpErr == nil && !newer && !o.Force:
			say("Up to date (%s).", o.Current)
			return nil
		}
	}
	if o.Check {
		say("Update available: %s -> %s\n%s", o.Current, rel.Tag, rel.URL)
		if n := strings.TrimSpace(rel.Notes); n != "" {
			say("\n%s", n)
		}
		return nil
	}

	say("Installing %s ...", rel.Tag)
	done, e := c.Install(ctx, rel, exe)
	if e != nil {
		return e
	}
	say("Installed %s at %s.", done.Tag, exe)
	if done.Previous != "" {
		say("The previous version is kept as %s; go back with: %s -rollback", done.Previous, BinaryName)
	}
	say("Release notes: %s", rel.URL)
	say("If the notes mention changes to the system setup, run: %s -install", BinaryName)
	return o.restart(say)
}

func (o Options) restart(say func(string, ...any)) error {
	if o.NoRestart {
		say("Restart it to use the new version: systemctl restart %s", o.Service)
		return nil
	}
	if e := RestartService(o.Service); e != nil {
		// Not installed as a service, or not running as root: not a failed update.
		say("Could not restart the service (%v). Restart it yourself: systemctl restart %s", e, o.Service)
		return nil
	}
	say("Restarted %s.", o.Service)
	return nil
}
