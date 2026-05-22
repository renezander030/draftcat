package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/ledongthuc/pdf"
)

// PDFParser is stateless; the type exists for symmetry with the other
// connectors and so future additions (OCR fallback, font allowlist) have
// a place to live without churning call sites.
type PDFParser struct {
	MaxPages int // 0 = no cap; non-zero halts ingest and returns error
}

func NewPDFParser() *PDFParser { return &PDFParser{MaxPages: 300} }

type PDFDoc struct {
	Filename string    `json:"filename"`
	Pages    []PDFPage `json:"pages"`
}

type PDFPage struct {
	Num    int        `json:"num"`
	Width  float64    `json:"width"`
	Height float64    `json:"height"`
	Text   string     `json:"text"`
	Items  []TextItem `json:"items"`
}

// TextItem coords use a top-left origin (Y is flipped from raw PDF). Matches
// what a UI overlay needs and what LiteParse emits.
type TextItem struct {
	Text     string  `json:"text"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	W        float64 `json:"w"`
	H        float64 `json:"h"`
	FontName string  `json:"font_name"`
	FontSize float64 `json:"font_size"`
}

type Span struct {
	Page     int     `json:"page"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	W        float64 `json:"w"`
	H        float64 `json:"h"`
	Strategy string  `json:"strategy"`
}

func (p *PDFParser) Extract(path string) (*PDFDoc, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("pdf open %s: %w", path, err)
	}
	defer f.Close()

	doc := &PDFDoc{Filename: filepath.Base(path)}
	n := r.NumPage()
	if p.MaxPages > 0 && n > p.MaxPages {
		return nil, fmt.Errorf("pdf %s exceeds max_pages (%d > %d)", path, n, p.MaxPages)
	}

	for i := 1; i <= n; i++ {
		page := r.Page(i)
		if page.V.IsNull() {
			continue
		}
		mb := page.V.Key("MediaBox")
		pageW := mb.Index(2).Float64()
		pageH := mb.Index(3).Float64()

		c := page.Content()
		items := make([]TextItem, 0, len(c.Text))
		var textBuf strings.Builder
		for _, t := range c.Text {
			items = append(items, TextItem{
				Text:     t.S,
				X:        t.X,
				Y:        pageH - (t.Y + t.FontSize),
				W:        t.W,
				H:        t.FontSize,
				FontName: t.Font,
				FontSize: t.FontSize,
			})
			textBuf.WriteString(t.S)
			textBuf.WriteByte(' ')
		}
		doc.Pages = append(doc.Pages, PDFPage{
			Num:    i,
			Width:  pageW,
			Height: pageH,
			Text:   strings.TrimSpace(textBuf.String()),
			Items:  items,
		})
	}
	return doc, nil
}

// FindSpan locates needle on page using a four-layer match: exact >
// whitespace-flexible > currency-stripped > alphanumeric-only. Returns the
// union bounding box of the matched fragments.
func (p *PDFParser) FindSpan(doc *PDFDoc, page int, needle string) (*Span, bool) {
	if page < 1 || page > len(doc.Pages) {
		return nil, false
	}
	pg := doc.Pages[page-1]
	for _, strat := range []struct {
		name string
		norm func(string) string
	}{
		{"exact", strings.ToLower},
		{"whitespace", collapseWS},
		{"currency", stripCurrency},
		{"alphanum", alphanumOnly},
	} {
		needleN := strat.norm(needle)
		if needleN == "" {
			continue
		}
		if span := scanItems(pg, strat.norm, needleN, strat.name); span != nil {
			return span, true
		}
	}
	return nil, false
}

// FormatPDFForPrompt is the AI-step input formatter. Page markers let the
// model emit `<cite page="N">` tags. Per-page chars are capped to keep token
// budgets sane; raise the cap if a pipeline needs full fidelity.
func FormatPDFForPrompt(doc *PDFDoc) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Document: %s (%d pages)\n\n", doc.Filename, len(doc.Pages))
	for _, pg := range doc.Pages {
		fmt.Fprintf(&b, "--- Page %d ---\n%s\n\n", pg.Num, truncatePDF(pg.Text, 4000))
	}
	return b.String()
}

func scanItems(pg PDFPage, norm func(string) string, needle, strat string) *Span {
	var concat strings.Builder
	type cursor struct{ start, end int }
	idx := make([]cursor, 0, len(pg.Items))
	for _, it := range pg.Items {
		s := norm(it.Text + " ")
		idx = append(idx, cursor{concat.Len(), concat.Len() + len(s)})
		concat.WriteString(s)
	}
	hit := strings.Index(concat.String(), needle)
	if hit < 0 {
		return nil
	}
	end := hit + len(needle)

	minX, minY := 1e9, 1e9
	maxX, maxY := -1e9, -1e9
	matched := false
	for i, cur := range idx {
		if cur.end <= hit || cur.start >= end {
			continue
		}
		matched = true
		it := pg.Items[i]
		if it.X < minX {
			minX = it.X
		}
		if it.Y < minY {
			minY = it.Y
		}
		if it.X+it.W > maxX {
			maxX = it.X + it.W
		}
		if it.Y+it.H > maxY {
			maxY = it.Y + it.H
		}
	}
	if !matched {
		return nil
	}
	return &Span{Page: pg.Num, X: minX, Y: minY, W: maxX - minX, H: maxY - minY, Strategy: strat}
}

var pdfWS = regexp.MustCompile(`\s+`)

func collapseWS(s string) string { return pdfWS.ReplaceAllString(strings.ToLower(s), " ") }

func stripCurrency(s string) string {
	return strings.NewReplacer("$", "", "€", "", "£", "", ",", "").Replace(collapseWS(s))
}

func alphanumOnly(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func truncatePDF(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
