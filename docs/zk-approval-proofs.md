# Zero-knowledge approval proofs

Draftcat can prove a useful fact about an approval without exporting the approval row itself:

> A hidden Draftcat approval record says `approve`, its required quorum is at least one, its observed approvals meet that quorum, and the record is bound to this pinned Draftcat instance key.

The proof is a selective-disclosure receipt. It is useful when a customer or auditor needs evidence that a human gate ran but should not receive customer content, reviewer identities, or internal workflow details.

## Use it when / skip it when

Use it when:

- the verifier already trusts a Draftcat instance key but should not see the underlying approval row;
- sharing the normal audit receipt would reveal customer or operator data;
- you want to evaluate a privacy-preserving audit handoff before designing a production ceremony.

Skip it when:

- the verifier needs the actual message, reviewer, timestamp, or vote count;
- the verifier does not have a trusted way to pin the instance-key commitment;
- a normal signed receipt already reveals nothing sensitive;
- you need an audited or production-ready compliance control today.

## Quick walkthrough

The approval row must already be signed with `DRAFTCAT_APPROVAL_SECRET`. The proof command first verifies that HMAC receipt and refuses an unsigned or altered row. Because the proof publishes a deterministic commitment that could be used to test guesses, this command requires at least 32 bytes and you should use a randomly generated secret, not a password.

1. The operator publishes the instance-key commitment through a trusted channel:

   ```bash
   # Existing instance: load the same secret that signed its approval rows.
   # New instance only: generate this once and keep it in your secret manager.
   export DRAFTCAT_APPROVAL_SECRET="$(openssl rand -hex 32)"
   ./draftcat zk-receipt key-id
   ```

   Save the printed 64-character commitment somewhere the verifier trusts. Do not copy it from the proof bundle you are about to verify; that would prove only that an unknown key made the proof.

2. The operator proves the latest direct human approval for a pipeline:

   ```bash
   ./draftcat zk-receipt prove \
     --out approval.proof.json \
     invoice-due-diligence
   ```

   `DRAFTCAT_STATE_PATH` takes precedence over `state.path` in `config.yaml`, matching the engine. Use `--config another.yaml` if needed. The output file is mode `0600` and contains no approval fields.

3. The verifier checks the bundle with the pinned commitment and a Draftcat binary built from the same circuit version:

   ```bash
   ./draftcat zk-receipt verify \
     --expect-key <pinned-key-commitment> \
     approval.proof.json
   ```

   A valid result means the circuit statement is true for some hidden witness bound to that key and receipt commitment. Verification needs neither the database nor `DRAFTCAT_APPROVAL_SECRET`.

## Public and private data

| Public in the proof bundle | Private witness |
| --- | --- |
| Schema, curve, proof type, and circuit ID | Pipeline and step |
| Pinned instance-key commitment | Decision time |
| Commitment to the complete receipt | Decision (`approve` is enforced in-circuit) |
| Groth16 proof and performance metadata | Operator ID and payload hash |
| | Required and observed quorum |
| | Nonce and instance secret |

The circuit uses domain-separated MiMC commitments over BN254 field elements. Strings and the instance secret are first reduced to field elements with domain-separated SHA-256. The receipt commitment covers every private field listed above.

## What the implementation refuses

- `policy_approve`: an automated policy decision is not presented as a human approval.
- `quorum_n < 1` or `quorum_got < quorum_n`: an absent or incomplete quorum cannot produce a proof.
- unsigned or HMAC-invalid rows: the CLI checks the existing action receipt before creating the witness.
- a proof from a different instance key: verification requires `--expect-key`.
- a different circuit build: the bundle's circuit ID must match the embedded verifying key.

Tests also change a public receipt commitment and confirm that verification fails.

## Trust model and limitations

This proof establishes a statement about data attested by the holder of the Draftcat instance secret. It does **not** independently establish that a real person clicked a button, that the person was authorized outside Draftcat, or that the approved action later executed. A compromised or dishonest key holder can attest false input, just as they can create an ordinary signed receipt. Protect the secret, pin the key commitment out of band, and rotate both after compromise.

The checked-in proving and verifying keys were produced with gnark's one-time `groth16.Setup` for this fixed circuit. This makes the preview work out of the box, but it is a single-party development setup. Before production, run a multiparty ceremony (or adopt a suitable transparent proof system), publish the resulting circuit and verifying-key hashes, and obtain an independent circuit and integration audit. gnark also notes that its implementations are provided without side-channel guarantees.

The proof intentionally discloses only the predicate. If a verifier needs to match the proof to a known action, extend the circuit with an agreed public action commitment rather than revealing the entire private row.

## Rebuild the development artifacts

Circuit changes require new artifacts and therefore a new circuit ID:

```bash
go run ./cmd/zkreceipt-setup
go test ./...
```

Commit all three files in `internal/zkreceipt/artifacts/` together. Never mix a constraint system, proving key, and verifying key from different setup runs.

The implementation uses [gnark](https://github.com/Consensys-Incorporated/gnark). Its documentation describes the one-time setup, proof, and verification flow in [Create and verify proofs](https://docs.gnark.consensys.io/HowTo/prove).
