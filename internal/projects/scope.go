package projects

import (
	"github.com/jplot/brain/internal/config"
	"github.com/jplot/brain/internal/db"
)

type Scope struct {
	Projects map[string]bool
	Current  string
}

var Everything = Scope{}

func ScopeFor(q db.Querier, cfg config.Config, cwd string) (Scope, error) {
	current, err := Locate(q, cfg, cwd)
	if err != nil {
		return Scope{}, err
	}
	if current == Roots {
		return Scope{Current: Roots}, nil
	}
	projects, err := Accessible(q, current)
	if err != nil {
		return Scope{}, err
	}
	projects[current] = true
	return Scope{projects, current}, nil
}
