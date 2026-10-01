package config

import (
	"encoding/json"
	"testing"
)

func TestExactNumericConstraintBoundary(t *testing.T) {
	max := float64(9007199254740992)
	c := ArgConstraint{Max: &max}
	for _, v := range []interface{}{json.Number("9007199254740993"), "9007199254740993", "NaN", "Inf", "-Inf", "1e-1000000000"} {
		if ok, _ := c.Check(v, true); ok {
			t.Errorf("unsafe boundary %v accepted", v)
		}
	}
	if ok, why := c.Check(json.Number("9007199254740992"), true); !ok {
		t.Fatalf("exact bound rejected: %s", why)
	}
	min := 1.1
	max = 1.1
	c = ArgConstraint{Min: &min, Max: &max}
	if ok, why := c.Check(json.Number("1.1"), true); !ok {
		t.Fatalf("decimal bound rejected: %s", why)
	}
}
