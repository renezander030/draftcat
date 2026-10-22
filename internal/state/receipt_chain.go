package state

import (
	"crypto/sha256"
	"strconv"
	"strings"
)

// ReceiptChainDomain seeds the receipt hash chain. link_0 is its SHA-256.
const ReceiptChainDomain = "draftcat-receipt-chain/v1"

// ReceiptChainGenesis returns link_0 of the receipt chain, the anchor of an
// empty store.
func ReceiptChainGenesis() [32]byte {
	return sha256.Sum256([]byte(ReceiptChainDomain))
}

// ReceiptChainBytes is the canonical form of one stored receipt as the chain
// consumes it: every column of the action_approvals row, row id and signature
// included, each value length-prefixed like the signed receipt envelopes so no
// shift of bytes between fields yields the same input. Unsigned rows are
// covered by the same full field set, so the chain binds their content too.
func ReceiptChainBytes(r ApprovalRecord) []byte {
	expires := int64(0)
	if !r.ExpiresAt.IsZero() {
		expires = r.ExpiresAt.Unix()
	}
	parts := []string{
		"row/1",
		strconv.FormatInt(r.ID, 10),
		strconv.Itoa(r.Version),
		r.ReceiptID,
		r.RunID,
		r.ActionID,
		r.Pipeline,
		r.Step,
		strconv.FormatInt(r.DecidedAt.Unix(), 10),
		r.Decision,
		strconv.FormatInt(r.OperatorID, 10),
		r.PayloadHash,
		strconv.Itoa(r.QuorumN),
		strconv.Itoa(r.QuorumGot),
		r.Nonce,
		r.Signature,
		r.Policy,
		r.PolicyHash,
		r.BindingHash,
		strconv.FormatInt(expires, 10),
		r.Lifecycle,
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(strconv.Itoa(len(p)))
		b.WriteByte(':')
		b.WriteString(p)
		b.WriteByte('|')
	}
	return []byte(b.String())
}

// NextReceiptLink returns link_n = sha256(link_{n-1} || ReceiptChainBytes(r)).
func NextReceiptLink(prev [32]byte, r ApprovalRecord) [32]byte {
	h := sha256.New()
	h.Write(prev[:])
	h.Write(ReceiptChainBytes(r))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// ApprovalsInStoredOrder returns every receipt row in the order the store
// appended them: ascending row id. The id is an AUTOINCREMENT key, so it only
// grows and is never reused; this is the order the receipt chain runs in.
func (s *StateStore) ApprovalsInStoredOrder() ([]ApprovalRecord, error) {
	return s.queryApprovals(`ORDER BY id ASC`)
}
