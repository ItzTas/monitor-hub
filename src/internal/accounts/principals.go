package accounts

import (
	"context"
	"slices"

	"golang.org/x/sync/errgroup"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/db"
)

type Me struct {
	Login       string                     `json:"login"`
	Admin       bool                       `json:"admin"`
	Permissions []string                   `json:"permissions"`
	Monitors    map[string][]access.Action `json:"monitors"`
}

func (s *Service) person(ctx context.Context, id int64, login string, admin bool) (access.Person, error) {
	grants, err := s.queries.UserGrants(ctx, []int64{id})
	if err != nil {
		return access.Person{}, err
	}
	granted := make([]access.Permission, 0, len(grants))
	for _, g := range grants {
		granted = append(granted, permission(g))
	}
	return access.Person{ID: id, Login: login, Admin: admin, Granted: granted}, nil
}

// Me lists the direct grants of p's owner and, for each monitor, what p can do there.
func (s *Service) Me(ctx context.Context, p access.Principal) (Me, error) {
	var (
		grants   []db.UserGrant
		monitors []string
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { grants, err = s.queries.UserGrants(gctx, []int64{p.UserID()}); return err })
	g.Go(func() (err error) { monitors, err = s.queries.MonitorNames(gctx); return err })
	if err := g.Wait(); err != nil {
		return Me{}, err
	}

	me := Me{
		Login:       p.Login(),
		Admin:       p.Admin(),
		Permissions: make([]string, 0, len(grants)),
		Monitors:    map[string][]access.Action{},
	}
	for _, grant := range grants {
		me.Permissions = append(me.Permissions, permission(grant).String())
	}
	// The database orders by monitor name in its own collation, not by slug.
	slices.Sort(me.Permissions)
	for _, name := range monitors {
		for _, a := range access.Actions() {
			if p.Can(a, name) {
				me.Monitors[name] = append(me.Monitors[name], a)
			}
		}
	}
	return me, nil
}
