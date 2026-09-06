// Package zkreceipt creates selective-disclosure proofs for Draftcat approval
// receipts. The public sees only an instance-key commitment and a commitment to
// the approved action; the approval row itself remains private.
package zkreceipt

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"strconv"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	nativemimc "github.com/consensys/gnark-crypto/ecc/bn254/fr/mimc"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/std/hash/mimc"
)

const (
	Schema    = "draftcat.zk-approval.v1"
	Curve     = "bn254"
	ProofType = "groth16"
)

// Record is the private approval witness. Only the two commitments derived
// from it are included in the proof's public inputs.
type Record struct {
	Pipeline    string
	Step        string
	DecidedAt   int64
	Decision    string
	OperatorID  int64
	PayloadHash string
	QuorumN     int
	QuorumGot   int
	Nonce       string
}

// Circuit proves that a committed approval was a human approval and that its
// declared quorum was met. All fields except the two commitments are private.
type Circuit struct {
	KeyCommitment     frontend.Variable `gnark:",public"`
	ReceiptCommitment frontend.Variable `gnark:",public"`

	Secret      frontend.Variable
	Pipeline    frontend.Variable
	Step        frontend.Variable
	DecidedAt   frontend.Variable
	Decision    frontend.Variable
	OperatorID  frontend.Variable
	PayloadHash frontend.Variable
	QuorumN     frontend.Variable
	QuorumGot   frontend.Variable
	Nonce       frontend.Variable
}

func (c *Circuit) Define(api frontend.API) error {
	// Bound the integers before comparing them in the scalar field.
	api.ToBinary(c.DecidedAt, 64)
	api.ToBinary(c.OperatorID, 64)
	api.ToBinary(c.QuorumN, 16)
	api.ToBinary(c.QuorumGot, 16)

	// Code 1 means a direct human "approve". Automated policy approvals use a
	// different decision in Draftcat and cannot satisfy this circuit.
	api.AssertIsEqual(c.Decision, 1)
	api.AssertIsLessOrEqual(1, c.QuorumN)
	api.AssertIsLessOrEqual(c.QuorumN, c.QuorumGot)

	keyHash, err := mimc.NewMiMC(api)
	if err != nil {
		return err
	}
	keyHash.Write(domainField("draftcat-instance-key-v1"), c.Secret)
	api.AssertIsEqual(c.KeyCommitment, keyHash.Sum())

	receiptHash, err := mimc.NewMiMC(api)
	if err != nil {
		return err
	}
	receiptHash.Write(
		domainField("draftcat-zk-approval-v1"),
		c.Secret,
		c.Pipeline,
		c.Step,
		c.DecidedAt,
		c.Decision,
		c.OperatorID,
		c.PayloadHash,
		c.QuorumN,
		c.QuorumGot,
		c.Nonce,
	)
	api.AssertIsEqual(c.ReceiptCommitment, receiptHash.Sum())
	return nil
}

func assignment(record Record, secret []byte) (*Circuit, *big.Int, *big.Int, error) {
	if len(secret) < 32 {
		return nil, nil, nil, fmt.Errorf("approval secret must be at least 32 bytes")
	}
	if record.Decision != "approve" {
		return nil, nil, nil, fmt.Errorf("decision must be a direct human approve, got %q", record.Decision)
	}
	if record.DecidedAt < 0 || record.OperatorID < 0 {
		return nil, nil, nil, fmt.Errorf("negative timestamp or operator ID is unsupported")
	}
	if record.QuorumN < 1 || record.QuorumN > 65535 || record.QuorumGot < record.QuorumN || record.QuorumGot > 65535 {
		return nil, nil, nil, fmt.Errorf("approval quorum is not satisfied or exceeds 16-bit bounds")
	}

	secretField := bytesField("secret", secret)
	values := []*big.Int{
		secretField,
		textField("pipeline", record.Pipeline),
		textField("step", record.Step),
		big.NewInt(record.DecidedAt),
		big.NewInt(1),
		big.NewInt(record.OperatorID),
		textField("payload-hash", record.PayloadHash),
		big.NewInt(int64(record.QuorumN)),
		big.NewInt(int64(record.QuorumGot)),
		textField("nonce", record.Nonce),
	}
	keyCommitment := nativeHash(domainField("draftcat-instance-key-v1"), secretField)
	receiptCommitment := nativeHash(append([]*big.Int{domainField("draftcat-zk-approval-v1")}, values...)...)

	return &Circuit{
		KeyCommitment:     keyCommitment,
		ReceiptCommitment: receiptCommitment,
		Secret:            values[0],
		Pipeline:          values[1],
		Step:              values[2],
		DecidedAt:         values[3],
		Decision:          values[4],
		OperatorID:        values[5],
		PayloadHash:       values[6],
		QuorumN:           values[7],
		QuorumGot:         values[8],
		Nonce:             values[9],
	}, keyCommitment, receiptCommitment, nil
}

func publicAssignment(keyCommitment, receiptCommitment *big.Int) *Circuit {
	return &Circuit{KeyCommitment: keyCommitment, ReceiptCommitment: receiptCommitment}
}

func domainField(value string) *big.Int { return textField("domain", value) }

func textField(label, value string) *big.Int {
	return bytesField(label, []byte(value))
}

func bytesField(label string, value []byte) *big.Int {
	h := sha256.New()
	_, _ = h.Write([]byte(label))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(value)
	var element fr.Element
	element.SetBytes(h.Sum(nil))
	return element.BigInt(new(big.Int))
}

func nativeHash(values ...*big.Int) *big.Int {
	h := nativemimc.NewMiMC()
	for _, value := range values {
		var element fr.Element
		element.SetBigInt(value)
		encoded := element.Bytes()
		_, _ = h.Write(encoded[:])
	}
	var result fr.Element
	result.SetBytes(h.Sum(nil))
	return result.BigInt(new(big.Int))
}

func encodeField(value *big.Int) string {
	var element fr.Element
	element.SetBigInt(value)
	encoded := element.Bytes()
	return fmt.Sprintf("%x", encoded[:])
}

func decodeField(value string) (*big.Int, error) {
	if len(value) != 64 {
		return nil, fmt.Errorf("field commitment must be 64 hex characters")
	}
	n, ok := new(big.Int).SetString(value, 16)
	if !ok || n.Sign() < 0 || n.Cmp(fr.Modulus()) >= 0 {
		return nil, fmt.Errorf("invalid field commitment %q", value)
	}
	return n, nil
}

// DescribeRecord is intentionally safe for errors: it identifies the row
// without printing its payload hash, operator ID, or nonce.
func DescribeRecord(record Record) string {
	return record.Pipeline + "/" + record.Step + " at " + strconv.FormatInt(record.DecidedAt, 10)
}
