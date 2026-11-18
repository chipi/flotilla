package models

import "testing"

// security T4: a routine's subset holds its aliases, their endpoints and prices, and nothing else.
func TestSubset(t *testing.T) {
	in, out := 1.0, 2.0
	c := &Config{Endpoints: map[string]Endpoint{"gw": {BaseURL: "http://gw"}, "model-host": {BaseURL: "http://model-host"}},
		Aliases: map[string]Alias{"a": {Endpoint: "gw", Model: "m1"}, "b": {Endpoint: "model-host", Model: "m2"}},
		Prices:  map[string]Price{"a": {InputPerMTok: &in, OutputPerMTok: &out}, "b": {InputPerMTok: &in, OutputPerMTok: &out}}}
	s, err := c.Subset([]string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Aliases) != 1 || len(s.Endpoints) != 1 || s.Endpoints["gw"].BaseURL != "http://gw" || len(s.Prices) != 1 {
		t.Fatalf("subset: %+v", s)
	}
	if _, err := c.Subset([]string{"a", "nope"}); err == nil {
		t.Fatal("an alias the config does not have was accepted")
	}
}
