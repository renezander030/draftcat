package main

import "testing"

func TestFindSpan_Layered(t *testing.T) {
	doc := &PDFDoc{
		Filename: "test.pdf",
		Pages: []PDFPage{{
			Num: 1, Width: 612, Height: 792,
			Items: []TextItem{
				{Text: "Subtotal", X: 72, Y: 410, W: 48, H: 11, FontSize: 11},
				{Text: "$12,450.00", X: 480, Y: 410, W: 64, H: 11, FontSize: 11},
				{Text: "Total", X: 72, Y: 442, W: 30, H: 11, FontSize: 11},
				{Text: "$13,695.00", X: 480, Y: 442, W: 64, H: 11, FontSize: 11},
			},
		}},
	}
	p := NewPDFParser()

	cases := []struct {
		name, needle, wantStrat string
	}{
		{"exact verbatim", "$13,695.00", "exact"},
		{"whitespace-flexible", "Total   $13,695.00", "whitespace"},
		{"currency stripped", "13695.00", "currency"},
		{"alphanumeric only", "Total $13695 00", "alphanum"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			span, ok := p.FindSpan(doc, 1, c.needle)
			if !ok {
				t.Fatalf("no span for %q", c.needle)
			}
			if span.Strategy != c.wantStrat {
				t.Errorf("strategy: got %s want %s", span.Strategy, c.wantStrat)
			}
			if span.W <= 0 || span.H <= 0 {
				t.Errorf("non-positive bbox: %+v", span)
			}
		})
	}

	if _, ok := p.FindSpan(doc, 1, "Net Income $999,999"); ok {
		t.Error("expected miss for needle absent from page")
	}
	if _, ok := p.FindSpan(doc, 99, "anything"); ok {
		t.Error("expected miss for out-of-range page")
	}
}

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
