// Package fhevote implements Draftcat's experimental encrypted vote tally.
//
// It uses the BGV scheme from Lattigo. Individual 0/1 votes are encrypted by
// the approvers, added by an untrusted collector, and decrypted only by the
// owner of the secret key. This package deliberately handles only the private
// computation. Existing Draftcat authentication decides which ballots are
// eligible, and the ZK receipt remains the transferable integrity proof.
package fhevote

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/tuneinsight/lattigo/v6/core/rlwe"
	"github.com/tuneinsight/lattigo/v6/schemes/bgv"
)

const (
	schemaPrivate   = "draftcat.fhe-vote-secret.v1"
	PublicKeySchema = "draftcat.fhe-vote-public.v1"
	BallotSchema    = "draftcat.fhe-vote-ballot.v1"
	TallySchema     = "draftcat.fhe-vote-tally.v1"
	Suite           = "LATTIGO_BGV_128_N12_QP109"
	maxBinaryBytes  = 8 << 20
	minBallots      = 3
	maxBallots      = 4096
)

// SecretKeyFile stays with the party allowed to read the final tally.
type SecretKeyFile struct {
	Schema          string `json:"schema"`
	Suite           string `json:"suite"`
	KeyID           string `json:"key_id"`
	SecretKeyBase64 string `json:"secret_key_base64"`
	PublicKeyBase64 string `json:"public_key_base64"`
}

// PublicKeyFile can be shared with every eligible voter and the collector.
type PublicKeyFile struct {
	Schema          string `json:"schema"`
	Suite           string `json:"suite"`
	KeyID           string `json:"key_id"`
	PublicKeyBase64 string `json:"public_key_base64"`
}

// Ballot is safe to give to the tally host: it contains an encrypted 0 or 1,
// not the readable vote. ContextID and BallotCommitment are hashes so workflow
// names and voter identities do not have to travel with the ciphertext.
type Ballot struct {
	Schema           string `json:"schema"`
	Suite            string `json:"suite"`
	KeyID            string `json:"key_id"`
	ContextID        string `json:"context_id"`
	BallotCommitment string `json:"ballot_commitment"`
	CiphertextBase64 string `json:"ciphertext_base64"`
	CiphertextBytes  int    `json:"ciphertext_bytes"`
}

// Tally is the homomorphic sum of a set of Ballots. The collector learns only
// how many encrypted ballots it accepted, not how any one of them voted.
type Tally struct {
	Schema           string   `json:"schema"`
	Suite            string   `json:"suite"`
	KeyID            string   `json:"key_id"`
	ContextID        string   `json:"context_id"`
	Ballots          int      `json:"ballots"`
	Contributors     []string `json:"contributors"`
	CiphertextBase64 string   `json:"ciphertext_base64"`
	CiphertextBytes  int      `json:"ciphertext_bytes"`
}

// Result is available only after decrypting the aggregate with SecretKeyFile.
type Result struct {
	Approvals int
	Ballots   int
}

func parameters() (bgv.Parameters, error) {
	// Lattigo's small-depth example parameter set is estimated at 128-bit
	// security. This preview needs additions only; it does not bootstrap or
	// claim that these example parameters are a production deployment profile.
	return bgv.NewParametersFromLiteral(bgv.ParametersLiteral{
		LogN:             12,
		LogQ:             []int{39, 31},
		LogP:             []int{39},
		PlaintextModulus: 0x10001,
	})
}

// GenerateKeys creates the key pair used for one encrypted tally domain.
func GenerateKeys() (SecretKeyFile, PublicKeyFile, error) {
	params, err := parameters()
	if err != nil {
		return SecretKeyFile{}, PublicKeyFile{}, err
	}
	sk, pk := rlwe.NewKeyGenerator(params).GenKeyPairNew()
	skBytes, err := sk.MarshalBinary()
	if err != nil {
		return SecretKeyFile{}, PublicKeyFile{}, fmt.Errorf("encode secret key: %w", err)
	}
	pkBytes, err := pk.MarshalBinary()
	if err != nil {
		return SecretKeyFile{}, PublicKeyFile{}, fmt.Errorf("encode public key: %w", err)
	}
	id := keyID(pkBytes)
	public := PublicKeyFile{
		Schema: PublicKeySchema, Suite: Suite, KeyID: id,
		PublicKeyBase64: base64.StdEncoding.EncodeToString(pkBytes),
	}
	secret := SecretKeyFile{
		Schema: schemaPrivate, Suite: Suite, KeyID: id,
		SecretKeyBase64: base64.StdEncoding.EncodeToString(skBytes),
		PublicKeyBase64: public.PublicKeyBase64,
	}
	return secret, public, nil
}

// EncryptVote encrypts approve as 1 and reject as 0. The readable context and
// ballot token are reduced to domain-separated commitments before serialization.
func EncryptVote(public PublicKeyFile, context, ballotToken string, approve bool) (Ballot, error) {
	params, pk, err := loadPublicKey(public)
	if err != nil {
		return Ballot{}, err
	}
	contextID, err := commitment("draftcat:fhe-vote:context:v1", context)
	if err != nil {
		return Ballot{}, fmt.Errorf("context: %w", err)
	}
	ballotID, err := commitment("draftcat:fhe-vote:ballot:v1", ballotToken)
	if err != nil {
		return Ballot{}, fmt.Errorf("ballot token: %w", err)
	}
	values := make([]uint64, params.MaxSlots())
	if approve {
		values[0] = 1
	}
	values[1] = 1
	plaintext := bgv.NewPlaintext(params, params.MaxLevel())
	if err := bgv.NewEncoder(params).Encode(values, plaintext); err != nil {
		return Ballot{}, fmt.Errorf("encode vote: %w", err)
	}
	ciphertext, err := rlwe.NewEncryptor(params, pk).EncryptNew(plaintext)
	if err != nil {
		return Ballot{}, fmt.Errorf("encrypt vote: %w", err)
	}
	encoded, err := ciphertext.MarshalBinary()
	if err != nil {
		return Ballot{}, fmt.Errorf("encode ciphertext: %w", err)
	}
	return Ballot{
		Schema: BallotSchema, Suite: Suite, KeyID: public.KeyID,
		ContextID: contextID, BallotCommitment: ballotID,
		CiphertextBase64: base64.StdEncoding.EncodeToString(encoded), CiphertextBytes: len(encoded),
	}, nil
}

// Aggregate adds encrypted ballots without decrypting any one of them.
func Aggregate(public PublicKeyFile, context string, ballots []Ballot) (Tally, error) {
	if len(ballots) < minBallots {
		return Tally{}, fmt.Errorf("at least %d encrypted ballots are required; smaller groups do not provide meaningful vote privacy", minBallots)
	}
	if len(ballots) > maxBallots {
		return Tally{}, fmt.Errorf("too many ballots: maximum is %d", maxBallots)
	}
	params, _, err := loadPublicKey(public)
	if err != nil {
		return Tally{}, err
	}
	contextID, err := commitment("draftcat:fhe-vote:context:v1", context)
	if err != nil {
		return Tally{}, fmt.Errorf("context: %w", err)
	}
	seen := make(map[string]struct{}, len(ballots))
	contributors := make([]string, 0, len(ballots))
	var total *rlwe.Ciphertext
	evaluator := bgv.NewEvaluator(params, nil)
	for i, ballot := range ballots {
		if err := validateBallot(ballot, public.KeyID, contextID); err != nil {
			return Tally{}, fmt.Errorf("ballot %d: %w", i+1, err)
		}
		if _, duplicate := seen[ballot.BallotCommitment]; duplicate {
			return Tally{}, fmt.Errorf("ballot %d repeats ballot commitment %s", i+1, ballot.BallotCommitment)
		}
		seen[ballot.BallotCommitment] = struct{}{}
		contributors = append(contributors, ballot.BallotCommitment)
		ciphertext, err := decodeCiphertext(params, ballot.CiphertextBase64, ballot.CiphertextBytes)
		if err != nil {
			return Tally{}, fmt.Errorf("ballot %d: %w", i+1, err)
		}
		if total == nil {
			total = ciphertext.CopyNew()
		} else if total, err = evaluator.AddNew(total, ciphertext); err != nil {
			return Tally{}, fmt.Errorf("add ballot %d: %w", i+1, err)
		}
	}
	encoded, err := total.MarshalBinary()
	if err != nil {
		return Tally{}, fmt.Errorf("encode encrypted tally: %w", err)
	}
	return Tally{
		Schema: TallySchema, Suite: Suite, KeyID: public.KeyID, ContextID: contextID,
		Ballots: len(ballots), Contributors: contributors,
		CiphertextBase64: base64.StdEncoding.EncodeToString(encoded), CiphertextBytes: len(encoded),
	}, nil
}

// Decrypt opens only the aggregate and rejects malformed or impossible totals.
func Decrypt(secret SecretKeyFile, context string, tally Tally) (Result, error) {
	params, sk, err := loadSecretKey(secret)
	if err != nil {
		return Result{}, err
	}
	contextID, err := commitment("draftcat:fhe-vote:context:v1", context)
	if err != nil {
		return Result{}, fmt.Errorf("context: %w", err)
	}
	if tally.Schema != TallySchema || tally.Suite != Suite {
		return Result{}, errors.New("unsupported encrypted tally schema or suite")
	}
	if tally.KeyID != secret.KeyID {
		return Result{}, errors.New("tally key ID does not match the secret key")
	}
	if tally.ContextID != contextID {
		return Result{}, errors.New("tally context does not match")
	}
	if tally.Ballots < minBallots || tally.Ballots > maxBallots || len(tally.Contributors) != tally.Ballots {
		return Result{}, errors.New("tally ballot count is out of range")
	}
	seen := make(map[string]struct{}, len(tally.Contributors))
	for _, contributor := range tally.Contributors {
		if !isSHA256(contributor) {
			return Result{}, errors.New("tally contains an invalid contributor commitment")
		}
		if _, duplicate := seen[contributor]; duplicate {
			return Result{}, errors.New("tally repeats a contributor commitment")
		}
		seen[contributor] = struct{}{}
	}
	ciphertext, err := decodeCiphertext(params, tally.CiphertextBase64, tally.CiphertextBytes)
	if err != nil {
		return Result{}, err
	}
	values := make([]uint64, params.MaxSlots())
	if err := bgv.NewEncoder(params).Decode(rlwe.NewDecryptor(params, sk).DecryptNew(ciphertext), values); err != nil {
		return Result{}, fmt.Errorf("decrypt tally: %w", err)
	}
	if values[1] != uint64(tally.Ballots) {
		return Result{}, errors.New("encrypted ballot count does not match the tally metadata")
	}
	if values[0] > values[1] {
		return Result{}, errors.New("decrypted approval count exceeds the number of ballots")
	}
	for _, value := range values[2:] {
		if value != 0 {
			return Result{}, errors.New("encrypted tally contains unexpected data outside the vote slot")
		}
	}
	return Result{Approvals: int(values[0]), Ballots: tally.Ballots}, nil // #nosec G115 -- bounded to maxBallots above.
}

func loadPublicKey(file PublicKeyFile) (bgv.Parameters, *rlwe.PublicKey, error) {
	params, err := parameters()
	if err != nil {
		return bgv.Parameters{}, nil, err
	}
	if file.Schema != PublicKeySchema || file.Suite != Suite {
		return bgv.Parameters{}, nil, errors.New("unsupported FHE public-key schema or suite")
	}
	encoded, err := decodeBase64(file.PublicKeyBase64, "public key")
	if err != nil {
		return bgv.Parameters{}, nil, err
	}
	if keyID(encoded) != file.KeyID {
		return bgv.Parameters{}, nil, errors.New("public-key ID mismatch")
	}
	publicKey := rlwe.NewPublicKey(params)
	if err := publicKey.UnmarshalBinary(encoded); err != nil {
		return bgv.Parameters{}, nil, fmt.Errorf("decode public key: %w", err)
	}
	return params, publicKey, nil
}

func loadSecretKey(file SecretKeyFile) (bgv.Parameters, *rlwe.SecretKey, error) {
	params, err := parameters()
	if err != nil {
		return bgv.Parameters{}, nil, err
	}
	if file.Schema != schemaPrivate || file.Suite != Suite {
		return bgv.Parameters{}, nil, errors.New("unsupported FHE secret-key schema or suite")
	}
	publicBytes, err := decodeBase64(file.PublicKeyBase64, "public key")
	if err != nil {
		return bgv.Parameters{}, nil, err
	}
	if keyID(publicBytes) != file.KeyID {
		return bgv.Parameters{}, nil, errors.New("secret file's public-key ID mismatch")
	}
	secretBytes, err := decodeBase64(file.SecretKeyBase64, "secret key")
	if err != nil {
		return bgv.Parameters{}, nil, err
	}
	secretKey := rlwe.NewSecretKey(params)
	if err := secretKey.UnmarshalBinary(secretBytes); err != nil {
		return bgv.Parameters{}, nil, fmt.Errorf("decode secret key: %w", err)
	}
	return params, secretKey, nil
}

func validateBallot(ballot Ballot, expectedKeyID, expectedContextID string) error {
	if ballot.Schema != BallotSchema || ballot.Suite != Suite {
		return errors.New("unsupported encrypted ballot schema or suite")
	}
	if ballot.KeyID != expectedKeyID {
		return errors.New("ballot key ID does not match the pinned public key")
	}
	if ballot.ContextID != expectedContextID {
		return errors.New("ballot context does not match")
	}
	if !isSHA256(ballot.BallotCommitment) {
		return errors.New("invalid ballot commitment")
	}
	return nil
}

func decodeCiphertext(params bgv.Parameters, value string, claimedBytes int) (*rlwe.Ciphertext, error) {
	encoded, err := decodeBase64(value, "ciphertext")
	if err != nil {
		return nil, err
	}
	if len(encoded) != claimedBytes {
		return nil, errors.New("ciphertext byte count does not match")
	}
	ciphertext := rlwe.NewCiphertext(params, 1, params.MaxLevel())
	if err := ciphertext.UnmarshalBinary(encoded); err != nil {
		return nil, fmt.Errorf("decode ciphertext: %w", err)
	}
	if ciphertext.Degree() != 1 || ciphertext.Level() != params.MaxLevel() {
		return nil, errors.New("ciphertext shape does not match the fixed FHE parameters")
	}
	return ciphertext, nil
}

func decodeBase64(value, label string) ([]byte, error) {
	if value == "" || len(value) > base64.StdEncoding.EncodedLen(maxBinaryBytes) {
		return nil, fmt.Errorf("%s is empty or too large", label)
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != value {
		return nil, fmt.Errorf("invalid %s encoding", label)
	}
	if len(decoded) > maxBinaryBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", label, maxBinaryBytes)
	}
	return decoded, nil
}

func commitment(domain, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("value must not be empty")
	}
	sum := sha256.Sum256([]byte(domain + "\x00" + value))
	return hex.EncodeToString(sum[:]), nil
}

func keyID(publicKey []byte) string {
	sum := sha256.Sum256(append([]byte("draftcat:fhe-vote:key:v1\x00"), publicKey...))
	return hex.EncodeToString(sum[:])
}

func isSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

// Marshal returns stable, indented JSON suitable for a command-line bundle.
func Marshal(value any) ([]byte, error) {
	return json.MarshalIndent(value, "", "  ")
}

// ParsePublicKey, ParseSecretKey, ParseBallot, and ParseTally reject unknown
// fields so typos do not silently change the trust boundary.
func ParsePublicKey(data []byte) (PublicKeyFile, error) {
	var value PublicKeyFile
	return value, strictJSON(data, &value)
}

func ParseSecretKey(data []byte) (SecretKeyFile, error) {
	var value SecretKeyFile
	return value, strictJSON(data, &value)
}

func ParseBallot(data []byte) (Ballot, error) {
	var value Ballot
	return value, strictJSON(data, &value)
}

func ParseTally(data []byte) (Tally, error) {
	var value Tally
	return value, strictJSON(data, &value)
}

func strictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode JSON: multiple values are not allowed")
		}
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}
