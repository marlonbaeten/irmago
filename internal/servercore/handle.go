package servercore

import (
	"time"

	"github.com/privacybydesign/gabi"
	"github.com/privacybydesign/gabi/big"
	"github.com/privacybydesign/gabi/revocation"
	"github.com/privacybydesign/irmago"
	"github.com/privacybydesign/irmago/server"
	"github.com/sirupsen/logrus"
)

// This file contains the handler functions for the protocol messages, receiving and returning normally
// Go-typed messages here (JSON (un)marshalling is handled by the router).
// Maintaining the session state is done here, as well as checking whether the session is in the
// appropriate status before handling the request.

func (session *session) handleDelete() {
	if session.status.Finished() {
		return
	}
	session.markAlive()

	session.result = &server.SessionResult{Token: session.token, Status: server.StatusCancelled, Type: session.action}
	session.setStatus(server.StatusCancelled)
}

func (session *session) handleGetRequest(min, max *irma.ProtocolVersion) (irma.SessionRequest, *irma.RemoteError) {
	if session.status != server.StatusInitialized {
		return nil, server.RemoteError(server.ErrorUnexpectedRequest, "Session already started")
	}

	session.markAlive()
	logger := session.conf.Logger.WithFields(logrus.Fields{"session": session.token})

	// we include the latest revocation records for the client here, as opposed to when the session
	// was started, so that the client always gets the very latest revocation records
	// TODO revocation database update mechanism
	var err error
	if err = session.request.Base().SetRevocationRecords(session.conf.IrmaConfiguration); err != nil {
		return nil, session.fail(server.ErrorUnknown, err.Error()) // TODO error type
	}

	// Handle legacy clients that do not support condiscon, by attempting to convert the condiscon
	// session request to the legacy session request format
	legacy, legacyErr := session.request.Legacy()
	session.legacyCompatible = legacyErr == nil
	if legacyErr != nil {
		logger.Info("Using condiscon: backwards compatibility with legacy IRMA apps is disabled")
	}

	if session.version, err = session.chooseProtocolVersion(min, max); err != nil {
		return nil, session.fail(server.ErrorProtocolVersion, "")
	}
	logger.WithFields(logrus.Fields{"version": session.version.String()}).Debugf("Protocol version negotiated")
	session.request.Base().ProtocolVersion = session.version

	session.setStatus(server.StatusConnected)

	if session.version.Below(2, 5) {
		logger.Info("Returning legacy session format")
		legacy.Base().ProtocolVersion = session.version
		return legacy, nil
	}

	// In case of issuance requests, strip revocation keys from []CredentialRequest
	isreq, issuing := session.request.(*irma.IssuanceRequest)
	if !issuing {
		return session.request, nil
	}
	cpy, err := copyObject(isreq)
	if err != nil {
		return nil, session.fail(server.ErrorUnknown, err.Error()) // TODO error type
	}
	for _, cred := range cpy.(*irma.IssuanceRequest).Credentials {
		cred.RevocationKey = ""
	}
	return cpy.(*irma.IssuanceRequest), nil
}

func (session *session) handleGetStatus() (server.Status, *irma.RemoteError) {
	return session.status, nil
}

func (session *session) handlePostSignature(signature *irma.SignedMessage) (*irma.ProofStatus, *irma.RemoteError) {
	if session.status != server.StatusConnected {
		return nil, server.RemoteError(server.ErrorUnexpectedRequest, "Session not yet started or already finished")
	}
	session.markAlive()

	var err error
	var rerr *irma.RemoteError
	session.result.Signature = signature
	session.result.Disclosed, session.result.ProofStatus, err = signature.Verify(
		session.conf.IrmaConfiguration, session.request.(*irma.SignatureRequest))
	if err == nil {
		session.setStatus(server.StatusDone)
	} else {
		if err == irma.ErrorMissingPublicKey {
			rerr = session.fail(server.ErrorUnknownPublicKey, err.Error())
		} else {
			rerr = session.fail(server.ErrorUnknown, err.Error())
		}
	}
	return &session.result.ProofStatus, rerr
}

func (session *session) handlePostDisclosure(disclosure *irma.Disclosure) (*irma.ProofStatus, *irma.RemoteError) {
	if session.status != server.StatusConnected {
		return nil, server.RemoteError(server.ErrorUnexpectedRequest, "Session not yet started or already finished")
	}
	session.markAlive()

	var err error
	var rerr *irma.RemoteError
	session.result.Disclosed, session.result.ProofStatus, err = disclosure.Verify(
		session.conf.IrmaConfiguration, session.request.(*irma.DisclosureRequest))
	if err == nil {
		session.setStatus(server.StatusDone)
	} else {
		if err == irma.ErrorMissingPublicKey {
			rerr = session.fail(server.ErrorUnknownPublicKey, err.Error())
		} else {
			rerr = session.fail(server.ErrorUnknown, err.Error())
		}
	}
	return &session.result.ProofStatus, rerr
}

func (session *session) handlePostCommitments(commitments *irma.IssueCommitmentMessage) ([]*gabi.IssueSignatureMessage, *irma.RemoteError) {
	if session.status != server.StatusConnected {
		return nil, server.RemoteError(server.ErrorUnexpectedRequest, "Session not yet started or already finished")
	}
	session.markAlive()

	request := session.request.(*irma.IssuanceRequest)

	discloseCount := len(commitments.Proofs) - len(request.Credentials)
	if discloseCount < 0 {
		return nil, session.fail(server.ErrorMalformedInput, "Received insufficient proofs")
	}

	// Compute list of public keys against which to verify the received proofs
	disclosureproofs := irma.ProofList(commitments.Proofs[:discloseCount])
	pubkeys, err := disclosureproofs.ExtractPublicKeys(session.conf.IrmaConfiguration)
	if err != nil {
		return nil, session.fail(server.ErrorMalformedInput, err.Error())
	}
	for _, cred := range request.Credentials {
		iss := cred.CredentialTypeID.IssuerIdentifier()
		pubkey, _ := session.conf.IrmaConfiguration.PublicKey(iss, cred.KeyCounter) // No error, already checked earlier
		pubkeys = append(pubkeys, pubkey)
	}

	// Verify and merge keyshare server proofs, if any
	for i, proof := range commitments.Proofs {
		pubkey := pubkeys[i]
		schemeid := irma.NewIssuerIdentifier(pubkey.Issuer).SchemeManagerIdentifier()
		if session.conf.IrmaConfiguration.SchemeManagers[schemeid].Distributed() {
			proofP, err := session.getProofP(commitments, schemeid)
			if err != nil {
				return nil, session.fail(server.ErrorKeyshareProofMissing, err.Error())
			}
			proof.MergeProofP(proofP, pubkey)
		}
	}

	// Verify all proofs and check disclosed attributes, if any, against request
	now := time.Now()
	session.result.Disclosed, session.result.ProofStatus, err = commitments.Disclosure().VerifyAgainstRequest(
		session.conf.IrmaConfiguration, request, request.GetContext(), request.GetNonce(nil), pubkeys, &now, false,
	)
	if err != nil {
		if err == irma.ErrorMissingPublicKey {
			return nil, session.fail(server.ErrorUnknownPublicKey, "")
		} else {
			return nil, session.fail(server.ErrorUnknown, "")
		}
	}
	if session.result.ProofStatus == irma.ProofStatusExpired {
		return nil, session.fail(server.ErrorAttributesExpired, "")
	}
	if session.result.ProofStatus != irma.ProofStatusValid {
		return nil, session.fail(server.ErrorInvalidProofs, "")
	}

	// Compute CL signatures
	var sigs []*gabi.IssueSignatureMessage
	for i, cred := range request.Credentials {
		id := cred.CredentialTypeID.IssuerIdentifier()
		pk, _ := session.conf.IrmaConfiguration.PublicKey(id, cred.KeyCounter)
		sk, _ := session.conf.PrivateKey(id)
		issuer := gabi.NewIssuer(sk, pk, one)
		proof, ok := commitments.Proofs[i+discloseCount].(*gabi.ProofU)
		if !ok {
			return nil, session.fail(server.ErrorMalformedInput, "Received invalid issuance commitment")
		}
		attributes, err := cred.AttributeList(session.conf.IrmaConfiguration, 0x03)
		if err != nil {
			return nil, session.fail(server.ErrorIssuanceFailed, err.Error())
		}

		var witness *revocation.Witness
		var nonrevAttr *big.Int
		if session.conf.IrmaConfiguration.CredentialTypes[cred.CredentialTypeID].SupportsRevocation() {
			db, err := session.conf.IrmaConfiguration.RevocationDB(cred.CredentialTypeID)
			if err != nil {
				return nil, session.fail(server.ErrorIssuanceFailed, err.Error())
			}
			if db.Enabled() {
				if witness, err = sk.RevocationGenerateWitness(&db.Current); err != nil {
					return nil, session.fail(server.ErrorIssuanceFailed, err.Error())
				}
				nonrevAttr = witness.E
				if err = db.AddIssuanceRecord(&revocation.IssuanceRecord{
					Key:        cred.RevocationKey,
					Attr:       nonrevAttr,
					Issued:     time.Now().UnixNano(), // or (floored) cred issuance time?
					ValidUntil: attributes.Expiry().UnixNano(),
				}); err != nil {
					return nil, session.fail(server.ErrorUnknown, "failed to save nonrevocation witness")
				}
			}
		}

		sig, err := issuer.IssueSignature(proof.U, attributes.Ints, nonrevAttr, commitments.Nonce2)
		if err != nil {
			return nil, session.fail(server.ErrorIssuanceFailed, err.Error())
		}
		sig.NonRevocationWitness = witness
		sigs = append(sigs, sig)
	}

	session.setStatus(server.StatusDone)
	return sigs, nil
}

func (s *Server) handlePostRevocationRecords(
	cred irma.CredentialTypeIdentifier, records []*revocation.Record,
) (interface{}, *irma.RemoteError) {
	db, err := s.conf.IrmaConfiguration.RevocationDB(cred)
	if err != nil {
		return nil, server.RemoteError(server.ErrorUnknown, err.Error()) // TODO error type
	}
	if err = db.AddRecords(records); err != nil {
		return nil, server.RemoteError(server.ErrorUnknown, err.Error()) // TODO error type
	}
	return nil, nil
}

func (s *Server) handleGetRevocationRecords(
	cred irma.CredentialTypeIdentifier, index int,
) ([]*revocation.Record, *irma.RemoteError) {
	if _, ok := s.conf.RevocationServers[cred]; ok {
		return nil, server.RemoteError(server.ErrorInvalidRequest, "not supported by this server")
	}
	db, err := s.conf.IrmaConfiguration.RevocationDB(cred)
	if err != nil {
		return nil, server.RemoteError(server.ErrorUnknown, err.Error()) // TODO error type
	}
	records, err := db.RevocationRecords(index)
	if err != nil {
		return nil, server.RemoteError(server.ErrorUnknown, err.Error()) // TODO error type
	}
	return records, nil
}
