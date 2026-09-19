// Package plan turns the box's Caddyfile into what a run backs up: the
// liveswap root, and what each app declares.
//
// The Caddyfile is written by whoever administers hotserve, which is
// not root, so nothing here is trusted because it parsed. A Plan is
// made by an unprivileged unit (Make) and handed to root as JSON, and
// root takes it only through Decode, which refuses anything it does
// not know and validates every field again.
package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/smallhoursorg/hotserve/liveswap/backupdecl"
)

// Plan is what a run backs up. Apps holds only the apps that declare a
// backup.
type Plan struct {
	Root string                        `json:"root"`
	Apps map[string]*backupdecl.Config `json:"apps"`
}

// Names returns the apps in the order a run takes them.
func (p *Plan) Names() []string {
	names := make([]string, 0, len(p.Apps))
	for n := range p.Apps {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Validate reports the first thing that makes the plan unusable.
func (p *Plan) Validate() error {
	switch {
	case strings.ContainsAny(p.Root, "{}"):
		return fmt.Errorf("the liveswap root %q is written with a placeholder ({ or }), which only hotserve's own environment can resolve; write the root as a literal path", p.Root)
	case !strings.HasPrefix(p.Root, "/"):
		return fmt.Errorf("the liveswap root %q is not an absolute path", p.Root)
	case path.Clean(p.Root) != p.Root:
		return fmt.Errorf("the liveswap root %q is not a clean path", p.Root)
	case strings.ContainsRune(p.Root, 0):
		return errors.New("the liveswap root contains a NUL")
	}
	for _, name := range p.Names() {
		if !backupdecl.ValidAppName(name) {
			return fmt.Errorf("app name %q is not one liveswap accepts", name)
		}
		decl := p.Apps[name]
		if decl == nil {
			return fmt.Errorf("app %s: the declaration is null", name)
		}
		if err := decl.Validate(); err != nil {
			return fmt.Errorf("app %s: backup %w", name, err)
		}
	}
	return nil
}

// Decode is how root takes a plan from the unit that made it: strict,
// and validated, whatever the maker says it checked.
func Decode(raw []byte) (*Plan, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	p := new(Plan)
	if err := dec.Decode(p); err != nil {
		return nil, fmt.Errorf("reading the plan: %w", err)
	}
	if dec.More() {
		return nil, errors.New("reading the plan: something follows it")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// FromAdapted reads a Plan out of what `hotserve adapt` prints: the
// config as written, in which an unset root is absent rather than
// defaulted.
func FromAdapted(raw []byte) (*Plan, error) {
	p, err := extract(raw)
	if err != nil {
		return nil, err
	}
	return p, p.Validate()
}

// extract is FromAdapted without the validation.
func extract(raw []byte) (*Plan, error) {
	var cfg struct {
		Apps struct {
			Liveswap *struct {
				Root string `json:"root"`
				Apps map[string]*struct {
					Backup *backupdecl.Config `json:"backup"`
				} `json:"apps"`
			} `json:"liveswap"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("reading the adapted config: %w", err)
	}
	p := &Plan{Root: backupdecl.DefaultRoot, Apps: map[string]*backupdecl.Config{}}
	ls := cfg.Apps.Liveswap
	if ls == nil {
		return p, nil
	}
	if ls.Root != "" {
		p.Root = ls.Root
	}
	for name, app := range ls.Apps {
		if app != nil && app.Backup != nil {
			p.Apps[name] = app.Backup
		}
	}
	return p, nil
}
