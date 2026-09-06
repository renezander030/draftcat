# Encrypted approval-vote tally

> Reviewers send encrypted ballots. The collector adds them without learning how anyone voted. Only the designated key owner decrypts the final total.

This preview adds the private-computation job that a zero-knowledge receipt does not do. The existing ZK feature hides an approval record while proving a claim about it to someone else. This feature instead lets an untrusted machine **operate on hidden inputs**.

An encrypted ballot file still travels to the collector. The promise is that the readable `approve` or `reject` value does not.

## Practical uses

- A buyer, supplier, and auditor must jointly release a payment, but none wants the shared workflow host to see its individual vote.
- An internal review panel votes on a sensitive customer escalation while a separate operations team runs the tally service.
- Several organizations approve a cross-company AI action and reveal only the final count to the person authorized to release it.

## Use it when / skip it when

Use it when:

- at least three eligible reviewers participate;
- the machine collecting the ballots is not allowed to read individual votes;
- one designated owner is allowed to learn the final count;
- Draftcat's existing authenticated channel controls which ballot invitations are eligible.

Skip it when:

- the Draftcat owner and tally collector are the same trusted party;
- one or two people vote, because the aggregate is too easy to attribute;
- the collector must learn the result directly;
- you need a compact proof for a customer or auditor—use [`zk-receipt`](zk-approval-proofs.md);
- you need a production-audited cryptographic control today.

## Walkthrough

### 1. The tally owner creates one campaign key

```bash
./draftcat fhe-vote keygen \
  --secret invoice-4821.fhe-secret.json \
  --public invoice-4821.fhe-public.json
```

The public file goes to eligible reviewers and the collector. The secret file is mode `0600`; keep it off the collector and out of source control. Use a fresh key for each sensitive campaign so the owner cannot compare overlapping partial tallies to infer a person's vote.

### 2. Each reviewer encrypts locally

```bash
./draftcat fhe-vote encrypt \
  --public invoice-4821.fhe-public.json \
  --context invoice-4821 \
  --ballot random-one-use-invitation-from-draftcat \
  --vote approve \
  --out alice.vote.json
```

`--context` binds all ballots to one action; only a domain-separated SHA-256 commitment is stored. `--ballot` must be a different, unpredictable invitation for each eligible reviewer. Its commitment lets the collector reject an exact replay without publishing a reviewer identity.

The encrypted file contains neither the context text, the invitation, nor an `approve`/`reject` field.

### 3. The collector combines ciphertexts

```bash
./draftcat fhe-vote tally \
  --public invoice-4821.fhe-public.json \
  --context invoice-4821 \
  --out invoice-4821.tally.json \
  alice.vote.json bob.vote.json carol.vote.json
```

The collector sees that three ballots were supplied and sees their opaque commitments. It homomorphically adds the encrypted vote and an encrypted `1` per submitted ballot; it has no decryption key. Commitments remain on the final tally for duplicate detection and operational reconciliation, but FHE does not authenticate that metadata.

### 4. The owner should open only the intended full-group total

```bash
./draftcat fhe-vote decrypt \
  --secret invoice-4821.fhe-secret.json \
  --context invoice-4821 \
  --expected 3 \
  --quorum 2 \
  invoice-4821.tally.json
```

Success prints, for example:

```text
QUORUM MET: 2 of 3 encrypted votes approved (required 2)
```

Exit code `0` means quorum was met. Exit code `3` means a structurally valid tally of the expected declared size did not meet quorum. Malformed, count-mismatched, wrong-key, or wrong-context input fails closed with exit code `1` or `2`.

## What each party learns

| Party | Learns | Does not learn |
| --- | --- | --- |
| Reviewer | Their own vote, campaign context, public key | Other votes, final result unless the owner shares it |
| Collector | Public-key ID, context commitment, opaque ballot commitments, number and size of ballots | Individual approve/reject values, secret key, final total |
| Key owner | Final approval count, submitted count, quorum result | Which encrypted ballot contained which vote, provided the owner never receives individual ballot files |

## Security boundary

Homomorphic encryption provides **confidentiality, not voter authentication or a proof of correct behavior**:

- The supplied CLI encrypts only `0` or `1`, but encryption alone does not prove a malicious voter used the CLI or chose an allowed value. The final check rejects an impossible total, not every possible compensating cheat. Accept ballots only through Draftcat's authenticated approval channel; a production protocol also needs a range proof for every encrypted vote.
- The ciphertext carries an encrypted contribution count, so editing only the visible count is detected and `--expected` catches an accidental partial tally. This is not proof of voter identity: anyone with the public key can fabricate a padding ballot. Authenticate every submission and reconcile invitation commitments outside this preview.
- The collector can modify ciphertexts because homomorphic encryption is intentionally malleable. The decrypt command rejects malformed shapes and impossible totals, but transport signatures are still required for production.
- Decrypting overlapping subsets can expose individual votes. Generate a fresh key per campaign and decrypt only the complete expected cohort.
- The secret key can decrypt an individual ballot too. Never give the key owner the ballot files; keep collection and decryption as separate roles.
- Context and ballot commitments can be guessed if their input values are predictable. Use a random campaign nonce and random one-use ballot invitations.
- File size, timing, public-key ID, and submitted ballot count remain visible metadata.

The current `zk-receipt` does not yet attest that an FHE tally drove a normal Draftcat action. Treat the two previews as separate until that binding is implemented and audited.

## Cryptographic implementation

The preview uses the BGV scheme in [Lattigo v6](https://github.com/tuneinsight/lattigo) with its small-depth example parameter set (`LogN=12`, `LogQP=109`, plaintext modulus `65537`), estimated by Lattigo at 128-bit security when published. Draftcat uses only encrypted addition: `approve` is encoded as `1`, `reject` as `0`, a second slot carries the contribution count, and the collector adds the ciphertexts.

BGV belongs to the FHE family, but this bounded preview does not perform bootstrapping or claim arbitrary-depth computation. Lattigo warns that its v6 API is fast-moving and that example parameters are for experimentation rather than a production deployment profile. Obtain an independent cryptographic and protocol audit before production reliance.

In the local end-to-end check used for this preview, one binary ciphertext was about 131 KB and its JSON ballot about 176 KB. That is far larger than a ZK receipt, but one ciphertext can pack thousands of values; encrypted aggregation is the intended use, not replacing a compact proof.
