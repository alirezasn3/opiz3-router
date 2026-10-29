package mnm

import (
	"context"

	"opiz3-router/internal/config"
)

// Rules is what the panel's rules card controls: whether the router keeps
// mini-net-man's rules equal to its own, and putting them back on demand.
type Rules struct {
	syncer  *Syncer
	cfgPath string
}

func NewRules(s *Syncer, cfgPath string) *Rules { return &Rules{syncer: s, cfgPath: cfgPath} }

// SetEnforce saves the setting in the config file and applies it at once.
func (r *Rules) SetEnforce(_ context.Context, on bool) error {
	if _, e := config.Modify(r.cfgPath, func(c *config.Config) { c.MiniNetMan.Enforce = on }); e != nil {
		return e
	}
	r.syncer.SetEnforce(on)
	return nil
}

// Restore puts the router's rules back in mini-net-man once.
func (r *Rules) Restore(ctx context.Context) error {
	_, e := r.syncer.Restore(ctx)
	return e
}
