package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	statestore "github.com/renezander030/draftcat/internal/state"
)

// receiptAnchorFormat names the anchor document layout.
const receiptAnchorFormat = "draftcat-receipt-anchor/v1"

// anchorTrailHex is how much of each link the trail keeps: enough to name the
// first diverging position, while the terminal link stays full length.
const anchorTrailHex = 16

// receiptAnchor is the terminal link of the receipt chain plus what an
// operator needs to read it: how many receipts it covers and where they end.
// The trail lists each covered receipt with a prefix of its link, so a later
// check can name the position where the stored history stops matching.
type receiptAnchor struct {
	Format         string        `json:"format"`
	Count          int           `json:"count"`
	FirstReceiptID string        `json:"first_receipt_id,omitempty"`
	LastReceiptID  string        `json:"last_receipt_id,omitempty"`
	LastDecidedAt  string        `json:"last_decided_at,omitempty"`
	Link           string        `json:"link"`
	Trail          []anchorEntry `json:"trail"`
}

type anchorEntry struct {
	ReceiptID string `json:"receipt_id"`
	Link      string `json:"link"`
}

// anchorResult is the outcome of checking the store against an anchor.
type anchorResult struct {
	Status          string `json:"status"` // match | mismatch
	AnchorCount     int    `json:"anchor_count"`
	StoreCount      int    `json:"store_count"`
	Appended        int    `json:"appended"`
	Position        int    `json:"position,omitempty"` // 1-based, first diverging receipt
	Kind            string `json:"kind,omitempty"`     // deleted | inserted | reordered | changed
	AnchorReceiptID string `json:"anchor_receipt_id,omitempty"`
	StoreReceiptID  string `json:"store_receipt_id,omitempty"`
	Link            string `json:"link"`
	Detail          string `json:"detail"`
}

func formatLink(l [32]byte) string { return "sha256:" + hex.EncodeToString(l[:]) }

// receiptChainLinks returns link_0..link_n over rows in stored order.
func receiptChainLinks(rows []statestore.ApprovalRecord) [][32]byte {
	links := make([][32]byte, 0, len(rows)+1)
	links = append(links, statestore.ReceiptChainGenesis())
	for _, r := range rows {
		links = append(links, statestore.NextReceiptLink(links[len(links)-1], r))
	}
	return links
}

// buildReceiptAnchor anchors every stored receipt.
func buildReceiptAnchor(rows []statestore.ApprovalRecord) receiptAnchor {
	links := receiptChainLinks(rows)
	a := receiptAnchor{Format: receiptAnchorFormat, Count: len(rows), Link: formatLink(links[len(rows)]), Trail: []anchorEntry{}}
	for i, r := range rows {
		a.Trail = append(a.Trail, anchorEntry{ReceiptID: receiptID(r), Link: hex.EncodeToString(links[i+1][:])[:anchorTrailHex]})
	}
	if len(rows) > 0 {
		a.FirstReceiptID = receiptID(rows[0])
		a.LastReceiptID = receiptID(rows[len(rows)-1])
		a.LastDecidedAt = rows[len(rows)-1].DecidedAt.UTC().Format(time.RFC3339)
	}
	return a
}

func validLink(s string) bool {
	h, ok := strings.CutPrefix(s, "sha256:")
	if !ok || len(h) != 64 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil && h == strings.ToLower(h)
}

// parseReceiptAnchor reads an anchor given as a bare terminal link
// (sha256:...), a JSON file written by `receipts anchor --json`, or - for
// stdin. A bare link carries no count; Count is -1 then.
func parseReceiptAnchor(arg string, stdin io.Reader) (receiptAnchor, error) {
	if strings.HasPrefix(arg, "sha256:") {
		if !validLink(arg) {
			return receiptAnchor{}, fmt.Errorf("anchor %q is not sha256: followed by 64 lowercase hex digits", arg)
		}
		return receiptAnchor{Count: -1, Link: arg}, nil
	}
	var body []byte
	var err error
	if arg == "-" {
		body, err = io.ReadAll(io.LimitReader(stdin, 64<<20))
	} else {
		body, err = os.ReadFile(arg) // #nosec G304 -- the operator names the anchor file
	}
	if err != nil {
		return receiptAnchor{}, err
	}
	var a receiptAnchor
	if err := decodeStrictJSON(body, &a); err != nil {
		return receiptAnchor{}, fmt.Errorf("anchor file: %w", err)
	}
	switch {
	case a.Format != receiptAnchorFormat:
		return a, fmt.Errorf("anchor file: format %q, want %q", a.Format, receiptAnchorFormat)
	case a.Count < 0:
		return a, fmt.Errorf("anchor file: count must not be negative")
	case !validLink(a.Link):
		return a, fmt.Errorf("anchor file: link is not sha256: followed by 64 lowercase hex digits")
	case len(a.Trail) != 0 && len(a.Trail) != a.Count:
		return a, fmt.Errorf("anchor file: trail holds %d entries for %d receipts", len(a.Trail), a.Count)
	}
	if n := len(a.Trail); n > 0 && !strings.HasPrefix(a.Link, "sha256:"+a.Trail[n-1].Link) {
		return a, fmt.Errorf("anchor file: the last trail entry does not match the link")
	}
	return a, nil
}

// checkReceiptAnchor recomputes the chain over the stored receipts and
// compares it with the anchor. Receipts after the anchored count are appended
// history and do not fail the check.
func checkReceiptAnchor(rows []statestore.ApprovalRecord, a receiptAnchor) anchorResult {
	links := receiptChainLinks(rows)
	res := anchorResult{Status: "mismatch", AnchorCount: a.Count, StoreCount: len(rows), Link: a.Link}
	if a.Count < 0 {
		// A bare link: find the prefix of the store that reproduces it.
		for i, l := range links {
			if formatLink(l) == a.Link {
				res.Status, res.AnchorCount, res.Appended = "match", i, len(rows)-i
				res.Detail = fmt.Sprintf("%d receipt(s) match the anchor; %d appended since", i, len(rows)-i)
				return res
			}
		}
		res.AnchorCount = 0
		res.Detail = fmt.Sprintf("no prefix of the %d stored receipt(s) reproduces the anchor", len(rows))
		return res
	}
	if len(a.Trail) == a.Count {
		for p := 1; p <= a.Count; p++ {
			want := a.Trail[p-1]
			if p <= len(rows) && receiptID(rows[p-1]) == want.ReceiptID &&
				hex.EncodeToString(links[p][:])[:anchorTrailHex] == want.Link {
				continue
			}
			classifyDivergence(&res, rows, a, p)
			return res
		}
	}
	if a.Count <= len(rows) && formatLink(links[a.Count]) == a.Link {
		res.Status, res.Appended = "match", len(rows)-a.Count
		res.Detail = fmt.Sprintf("%d receipt(s) match the anchor; %d appended since", a.Count, res.Appended)
		return res
	}
	if a.Count > len(rows) {
		res.Detail = fmt.Sprintf("the store holds %d receipt(s), the anchor covers %d", len(rows), a.Count)
	} else {
		res.Detail = fmt.Sprintf("the first %d stored receipt(s) do not reproduce the anchor", a.Count)
	}
	if len(a.Trail) == a.Count && a.Count > 0 {
		res.Position, res.Kind = a.Count, "changed"
		res.AnchorReceiptID, res.StoreReceiptID = a.Trail[a.Count-1].ReceiptID, receiptID(rows[a.Count-1])
	}
	return res
}

// classifyDivergence names what happened at position p, the first receipt
// whose id or link differs from the trail: the anchored receipt is gone
// (deleted), the stored one was never anchored (inserted), both exist but at
// other positions (reordered), or the same receipt now has other content
// (changed).
func classifyDivergence(res *anchorResult, rows []statestore.ApprovalRecord, a receiptAnchor, p int) {
	res.Position = p
	res.AnchorReceiptID = a.Trail[p-1].ReceiptID
	if p > len(rows) {
		res.Kind = "deleted"
		res.Detail = fmt.Sprintf("receipt %s is missing from the store", res.AnchorReceiptID)
		return
	}
	res.StoreReceiptID = receiptID(rows[p-1])
	if res.StoreReceiptID == res.AnchorReceiptID {
		res.Kind = "changed"
		res.Detail = fmt.Sprintf("receipt %s no longer matches its anchored content", res.StoreReceiptID)
		return
	}
	stored := make(map[string]bool, len(rows))
	for _, r := range rows {
		stored[receiptID(r)] = true
	}
	anchored := make(map[string]bool, len(a.Trail))
	for _, e := range a.Trail {
		anchored[e.ReceiptID] = true
	}
	switch {
	case !stored[res.AnchorReceiptID]:
		res.Kind = "deleted"
		res.Detail = fmt.Sprintf("receipt %s is missing from the store", res.AnchorReceiptID)
	case !anchored[res.StoreReceiptID]:
		res.Kind = "inserted"
		res.Detail = fmt.Sprintf("receipt %s was not in the anchored history", res.StoreReceiptID)
	default:
		res.Kind = "reordered"
		res.Detail = fmt.Sprintf("receipt %s stands where the anchor has %s", res.StoreReceiptID, res.AnchorReceiptID)
	}
}

func printAnchorResult(w io.Writer, res anchorResult) {
	if res.Status == "match" {
		_, _ = fmt.Fprintf(w, "anchor ok: %s (%s)\n", res.Detail, res.Link)
		return
	}
	if res.Position > 0 {
		_, _ = fmt.Fprintf(w, "anchor MISMATCH at position %d (%s): %s\n", res.Position, res.Kind, res.Detail)
		return
	}
	_, _ = fmt.Fprintf(w, "anchor MISMATCH: %s\n", res.Detail)
}

// loadStoredReceipts reads every receipt in chain order from a read-only store.
func loadStoredReceipts(configPath string, stderr io.Writer, cmd string) ([]statestore.ApprovalRecord, int) {
	st, closeStore, code := openStateForCmd(configPath)
	if code != 0 {
		return nil, code
	}
	defer closeStore()
	rows, err := st.ApprovalsInStoredOrder()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: read: %v\n", cmd, err)
		return nil, 1
	}
	return rows, 0
}

// runReceiptsAnchor prints the anchor of the whole receipt history.
func runReceiptsAnchor(args []string, stdout, stderr io.Writer) int {
	configPath, jsonOut := "config.yaml", false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--config", "-config":
			if i+1 >= len(args) {
				_, _ = fmt.Fprintln(stderr, "receipts anchor: --config requires a path")
				return 2
			}
			configPath = args[i+1]
			i++
		case "-h", "--help":
			_, _ = fmt.Fprintln(stdout, "Usage: draftcat receipts anchor [--json] [--config path]")
			return 0
		default:
			_, _ = fmt.Fprintf(stderr, "receipts anchor: unknown option %q\n", args[i])
			return 2
		}
	}
	rows, code := loadStoredReceipts(configPath, stderr, "receipts anchor")
	if code != 0 {
		return code
	}
	a := buildReceiptAnchor(rows)
	if jsonOut {
		b, _ := json.MarshalIndent(a, "", "  ")
		_, _ = fmt.Fprintln(stdout, string(b))
		return 0
	}
	_, _ = fmt.Fprintf(stdout, "receipts      %d\n", a.Count)
	if a.Count > 0 {
		_, _ = fmt.Fprintf(stdout, "first         %s\nlast          %s\nlast decided  %s\n", a.FirstReceiptID, a.LastReceiptID, a.LastDecidedAt)
	}
	_, _ = fmt.Fprintf(stdout, "anchor        %s\n", a.Link)
	return 0
}

// runReceiptsVerifyAnchor checks the stored history against an anchor.
func runReceiptsVerifyAnchor(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	configPath, jsonOut, anchorArg := "config.yaml", false, ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--config", "-config":
			if i+1 >= len(args) {
				_, _ = fmt.Fprintln(stderr, "receipts verify-anchor: --config requires a path")
				return 2
			}
			configPath = args[i+1]
			i++
		case "-h", "--help":
			_, _ = fmt.Fprintln(stdout, "Usage: draftcat receipts verify-anchor <sha256:...|anchor.json|-> [--json] [--config path]")
			return 0
		default:
			if anchorArg != "" || (strings.HasPrefix(args[i], "-") && args[i] != "-") {
				_, _ = fmt.Fprintf(stderr, "receipts verify-anchor: unexpected argument %q\n", args[i])
				return 2
			}
			anchorArg = args[i]
		}
	}
	if anchorArg == "" {
		_, _ = fmt.Fprintln(stderr, "Usage: draftcat receipts verify-anchor <sha256:...|anchor.json|-> [--json] [--config path]")
		return 2
	}
	a, err := parseReceiptAnchor(anchorArg, stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "receipts verify-anchor: %v\n", err)
		return 2
	}
	rows, code := loadStoredReceipts(configPath, stderr, "receipts verify-anchor")
	if code != 0 {
		return code
	}
	res := checkReceiptAnchor(rows, a)
	if jsonOut {
		b, _ := json.MarshalIndent(res, "", "  ")
		_, _ = fmt.Fprintln(stdout, string(b))
	} else {
		printAnchorResult(stdout, res)
	}
	if res.Status != "match" {
		return 1
	}
	return 0
}

// splitAnchorFlag removes --anchor <value> (or --anchor=value) from
// audit-verify's arguments, leaving its positional form untouched. ok is false
// when the flag is present without a value.
func splitAnchorFlag(args []string) (rest []string, anchor string, present, ok bool) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--anchor":
			if i+1 >= len(args) {
				return rest, "", true, false
			}
			anchor, present = args[i+1], true
			i++
		case strings.HasPrefix(args[i], "--anchor="):
			anchor, present = strings.TrimPrefix(args[i], "--anchor="), true
		default:
			rest = append(rest, args[i])
		}
	}
	return rest, anchor, present, !present || anchor != ""
}
