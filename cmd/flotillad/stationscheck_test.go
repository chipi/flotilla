package main

import (
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/models"
	"github.com/chipi/flotilla/internal/spec"
)

func TestCheckStationModels(t *testing.T) {
	cfg := &models.Config{Endpoints: map[string]models.Endpoint{"gw": {BaseURL: "http://gw/v1"}},
		Aliases: map[string]models.Alias{
			"native": {Endpoint: "gw", Model: "a", Capabilities: models.Capabilities{Tools: "native"}},
			"plain":  {Endpoint: "gw", Model: "b"}}}
	j := func(tools, model string) *spec.Routine {
		return &spec.Routine{Fleet: "f", Routine: "r", Stations: map[string]*spec.Station{"s": {Harness: "pi", Model: model, Tools: tools}}}
	}
	for name, c := range map[string]struct {
		r    *spec.Routine
		want string
	}{
		"fine":                {j("all", "native"), ""},
		"no tools on a plain": {j("none", "plain"), ""},
		"tools on a plain":    {j("read", "plain"), "whose tools capability is none"},
		"unknown alias":       {j("none", "ghost"), "not an alias"},
	} {
		err := checkStationModels(c.r, cfg)
		if (c.want == "") != (err == nil) || (c.want != "" && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
	if err := checkStationModels(j("none", "native"), nil); err == nil {
		t.Error("no models config: refused")
	}
}
