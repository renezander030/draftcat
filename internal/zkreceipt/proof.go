package zkreceipt

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark/backend/groth16"
	groth16bn254 "github.com/consensys/gnark/backend/groth16/bn254"
	"github.com/consensys/gnark/constraint"
	constraintbn254 "github.com/consensys/gnark/constraint/bn254"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/frontend/cs/r1cs"
)

// Bundle is safe to give to a verifier. It contains no approval fields: only
// public commitments, the proof, and non-sensitive performance metadata.
type Bundle struct {
	Schema            string `json:"schema"`
	Curve             string `json:"curve"`
	ProofType         string `json:"proof_type"`
	CircuitID         string `json:"circuit_id"`
	KeyCommitment     string `json:"key_commitment"`
	ReceiptCommitment string `json:"receipt_commitment"`
	Proof             string `json:"proof_base64"`
	ProofBytes        int    `json:"proof_bytes"`
	ProverMS          int64  `json:"prover_ms"`
}

//go:embed artifacts/approval.r1cs
var embeddedR1CS []byte

//go:embed artifacts/approval.pk
var embeddedPK []byte

//go:embed artifacts/approval.vk
var embeddedVK []byte

type proofSystem struct {
	ccs       constraint.ConstraintSystem
	pk        groth16.ProvingKey
	vk        groth16.VerifyingKey
	circuitID string
}

var (
	embeddedSystem     *proofSystem
	embeddedSystemErr  error
	embeddedSystemOnce sync.Once
)

func compileCircuit() (constraint.ConstraintSystem, error) {
	return frontend.Compile(ecc.BN254.ScalarField(), r1cs.NewBuilder, &Circuit{})
}

func loadEmbeddedSystem() (*proofSystem, error) {
	embeddedSystemOnce.Do(func() {
		ccs := new(constraintbn254.R1CS)
		if _, err := ccs.ReadFrom(bytes.NewReader(embeddedR1CS)); err != nil {
			embeddedSystemErr = fmt.Errorf("read embedded constraint system: %w", err)
			return
		}
		pk := new(groth16bn254.ProvingKey)
		if _, err := pk.ReadFrom(bytes.NewReader(embeddedPK)); err != nil {
			embeddedSystemErr = fmt.Errorf("read embedded proving key: %w", err)
			return
		}
		vk := new(groth16bn254.VerifyingKey)
		if _, err := vk.ReadFrom(bytes.NewReader(embeddedVK)); err != nil {
			embeddedSystemErr = fmt.Errorf("read embedded verifying key: %w", err)
			return
		}
		digest := sha256.Sum256(embeddedVK)
		embeddedSystem = &proofSystem{ccs: ccs, pk: pk, vk: vk, circuitID: hex.EncodeToString(digest[:])}
	})
	return embeddedSystem, embeddedSystemErr
}

// GenerateArtifacts compiles the fixed circuit and creates a Groth16 proving
// and verifying key. The checked-in keys make the preview reproducible; they
// are a single-party setup and must be replaced by a ceremony before production.
func GenerateArtifacts(directory string) error {
	ccs, err := compileCircuit()
	if err != nil {
		return fmt.Errorf("compile circuit: %w", err)
	}
	pk, vk, err := groth16.Setup(ccs)
	if err != nil {
		return fmt.Errorf("Groth16 setup: %w", err)
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	artifacts := []struct {
		name   string
		writer interface {
			WriteTo(io.Writer) (int64, error)
		}
	}{
		{name: "approval.r1cs", writer: ccs},
		{name: "approval.pk", writer: pk},
		{name: "approval.vk", writer: vk},
	}
	for _, artifact := range artifacts {
		var buffer bytes.Buffer
		if _, err := artifact.writer.WriteTo(&buffer); err != nil {
			return fmt.Errorf("serialize %s: %w", artifact.name, err)
		}
		if err := os.WriteFile(filepath.Join(directory, artifact.name), buffer.Bytes(), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", artifact.name, err)
		}
	}
	return nil
}

// Prove creates a selective-disclosure proof for an already authenticated
// approval row. Callers are responsible for checking the row's HMAC first.
func Prove(record Record, secret []byte) (Bundle, error) {
	system, err := loadEmbeddedSystem()
	if err != nil {
		return Bundle{}, err
	}
	privateAssignment, keyCommitment, receiptCommitment, err := assignment(record, secret)
	if err != nil {
		return Bundle{}, err
	}
	witness, err := frontend.NewWitness(privateAssignment, ecc.BN254.ScalarField())
	if err != nil {
		return Bundle{}, fmt.Errorf("build private witness: %w", err)
	}
	started := time.Now()
	proof, err := groth16.Prove(system.ccs, system.pk, witness)
	if err != nil {
		return Bundle{}, fmt.Errorf("prove approval: %w", err)
	}
	var encoded bytes.Buffer
	if _, err := proof.WriteTo(&encoded); err != nil {
		return Bundle{}, fmt.Errorf("serialize proof: %w", err)
	}
	return Bundle{
		Schema:            Schema,
		Curve:             Curve,
		ProofType:         ProofType,
		CircuitID:         system.circuitID,
		KeyCommitment:     encodeField(keyCommitment),
		ReceiptCommitment: encodeField(receiptCommitment),
		Proof:             base64.StdEncoding.EncodeToString(encoded.Bytes()),
		ProofBytes:        encoded.Len(),
		ProverMS:          time.Since(started).Milliseconds(),
	}, nil
}

// Verify checks the proof and, when expectedKey is non-empty, requires the
// proof to come from that pinned Draftcat instance key.
func Verify(bundle Bundle, expectedKey string) error {
	if bundle.Schema != Schema || bundle.Curve != Curve || bundle.ProofType != ProofType {
		return fmt.Errorf("unsupported proof bundle %q/%q/%q", bundle.Schema, bundle.Curve, bundle.ProofType)
	}
	system, err := loadEmbeddedSystem()
	if err != nil {
		return err
	}
	if bundle.CircuitID != system.circuitID {
		return fmt.Errorf("circuit ID does not match this Draftcat build")
	}
	if expectedKey != "" && bundle.KeyCommitment != expectedKey {
		return fmt.Errorf("instance key commitment mismatch")
	}
	keyCommitment, err := decodeField(bundle.KeyCommitment)
	if err != nil {
		return fmt.Errorf("key commitment: %w", err)
	}
	receiptCommitment, err := decodeField(bundle.ReceiptCommitment)
	if err != nil {
		return fmt.Errorf("receipt commitment: %w", err)
	}
	publicWitness, err := frontend.NewWitness(publicAssignment(keyCommitment, receiptCommitment), ecc.BN254.ScalarField(), frontend.PublicOnly())
	if err != nil {
		return fmt.Errorf("build public witness: %w", err)
	}
	proofBytes, err := base64.StdEncoding.DecodeString(bundle.Proof)
	if err != nil {
		return fmt.Errorf("decode proof: %w", err)
	}
	proof := new(groth16bn254.Proof)
	if _, err := proof.ReadFrom(bytes.NewReader(proofBytes)); err != nil {
		return fmt.Errorf("read proof: %w", err)
	}
	if err := groth16.Verify(proof, system.vk, publicWitness); err != nil {
		return fmt.Errorf("invalid zero-knowledge approval proof: %w", err)
	}
	return nil
}

func KeyCommitment(secret []byte) (string, error) {
	if len(secret) < 32 {
		return "", fmt.Errorf("approval secret must be at least 32 bytes")
	}
	return encodeField(nativeHash(domainField("draftcat-instance-key-v1"), bytesField("secret", secret))), nil
}

func MarshalBundle(bundle Bundle) ([]byte, error) {
	return json.MarshalIndent(bundle, "", "  ")
}

func UnmarshalBundle(data []byte) (Bundle, error) {
	var bundle Bundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return Bundle{}, fmt.Errorf("parse proof bundle: %w", err)
	}
	return bundle, nil
}
