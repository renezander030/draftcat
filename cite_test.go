package main

import "testing"

func TestCiteTagRegex(t *testing.T) {
	raw := `The total was <cite file="acme.pdf" page="1">$13,695.00</cite> per the invoice, ` +
		`with subtotal <cite file="acme.pdf" page="1">$12,450.00</cite>.`
	matches := citeTagRe.FindAllStringSubmatch(raw, -1)
	if len(matches) != 2 {
		t.Fatalf("got %d matches, want 2", len(matches))
	}
	if matches[0][1] != "acme.pdf" || matches[0][2] != "1" || matches[0][3] != "$13,695.00" {
		t.Errorf("first match: %+v", matches[0])
	}
}
