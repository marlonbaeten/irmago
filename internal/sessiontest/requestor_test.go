package sessiontest

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/http"

	"crypto/rand"
	"path/filepath"

	"reflect"

	"testing"

	"github.com/privacybydesign/gabi/big"
	"github.com/privacybydesign/gabi/revocation"
	"github.com/privacybydesign/irmago"
	"github.com/privacybydesign/irmago/internal/test"
	"github.com/privacybydesign/irmago/irmaclient"
	"github.com/privacybydesign/irmago/server"
	"github.com/stretchr/testify/require"
)

type sessionOption int

const (
	sessionOptionUpdatedIrmaConfiguration sessionOption = 1 << iota
	sessionOptionUnsatisfiableRequest
	sessionOptionRetryPost
	sessionOptionIgnoreClientError
)

type requestorSessionResult struct {
	*server.SessionResult
	Missing irmaclient.MissingAttributes
}

func requestorSessionHelper(t *testing.T, request irma.SessionRequest, client *irmaclient.Client, options ...sessionOption) *requestorSessionResult {
	if client == nil {
		client, _ = parseStorage(t)
		defer test.ClearTestStorage(t)
	}

	StartIrmaServer(t, len(options) == 1 && options[0] == sessionOptionUpdatedIrmaConfiguration)
	defer StopIrmaServer()

	clientChan := make(chan *SessionResult)
	serverChan := make(chan *server.SessionResult)

	qr, token, err := irmaServer.StartSession(request, func(result *server.SessionResult) {
		serverChan <- result
	})
	require.NoError(t, err)

	opts := 0
	for _, o := range options {
		opts |= int(o)
	}

	var h irmaclient.Handler
	if opts&int(sessionOptionUnsatisfiableRequest) > 0 {
		h = &UnsatisfiableTestHandler{TestHandler{t, clientChan, client, nil, ""}}
	} else {
		h = &TestHandler{t, clientChan, client, nil, ""}
	}

	j, err := json.Marshal(qr)
	require.NoError(t, err)
	client.NewSession(string(j), h)
	clientResult := <-clientChan
	if (len(options) == 0 || options[0] != sessionOptionIgnoreClientError) && clientResult != nil {
		require.NoError(t, clientResult.Err)
	}

	if opts&int(sessionOptionUnsatisfiableRequest) > 0 {
		require.NotNil(t, clientResult)
		return &requestorSessionResult{nil, clientResult.Missing}
	}

	serverResult := <-serverChan
	require.Equal(t, token, serverResult.Token)

	if opts&int(sessionOptionRetryPost) > 0 {
		req, err := http.NewRequest(http.MethodPost,
			qr.URL+"/proofs",
			bytes.NewBuffer([]byte(h.(*TestHandler).result)),
		)
		require.NoError(t, err)
		req.Header.Add("Content-Type", "application/json")
		res, err := new(http.Client).Do(req)
		require.NoError(t, err)
		require.True(t, res.StatusCode < 300)
		_, err = ioutil.ReadAll(res.Body)
		require.NoError(t, err)
	}

	return &requestorSessionResult{serverResult, nil}
}

// Check that nonexistent IRMA identifiers in the session request fail the session
func TestRequestorInvalidRequest(t *testing.T) {
	StartIrmaServer(t, false)
	defer StopIrmaServer()
	_, _, err := irmaServer.StartSession(irma.NewDisclosureRequest(
		irma.NewAttributeTypeIdentifier("irma-demo.RU.foo.bar"),
		irma.NewAttributeTypeIdentifier("irma-demo.baz.qux.abc"),
	), nil)
	require.Error(t, err)
}

func TestRequestorDoubleGET(t *testing.T) {
	StartIrmaServer(t, false)
	defer StopIrmaServer()
	qr, _, err := irmaServer.StartSession(irma.NewDisclosureRequest(
		irma.NewAttributeTypeIdentifier("irma-demo.RU.studentCard.studentID"),
	), nil)
	require.NoError(t, err)

	// Simulate the first GET by the client in the session protocol, twice
	var o interface{}
	transport := irma.NewHTTPTransport(qr.URL)
	transport.SetHeader(irma.MinVersionHeader, "2.5")
	transport.SetHeader(irma.MaxVersionHeader, "2.5")
	require.NoError(t, transport.Get("", &o))
	require.NoError(t, transport.Get("", &o))
}

func TestRequestorSignatureSession(t *testing.T) {
	client, _ := parseStorage(t)
	id := irma.NewAttributeTypeIdentifier("irma-demo.RU.studentCard.studentID")

	var serverResult *requestorSessionResult
	for _, opt := range []sessionOption{0, sessionOptionRetryPost} {
		serverResult = requestorSessionHelper(t, irma.NewSignatureRequest("message", id), client, opt)

		require.Nil(t, serverResult.Err)
		require.Equal(t, irma.ProofStatusValid, serverResult.ProofStatus)
		require.NotEmpty(t, serverResult.Disclosed)
		require.Equal(t, id, serverResult.Disclosed[0][0].Identifier)
		require.Equal(t, "456", serverResult.Disclosed[0][0].Value["en"])
	}

	// Load the updated scheme in which an attribute was added to the studentCard credential type
	schemeid := irma.NewSchemeManagerIdentifier("irma-demo")
	client.Configuration.SchemeManagers[schemeid].URL = "http://localhost:48681/irma_configuration_updated/irma-demo"
	require.NoError(t, client.Configuration.UpdateSchemeManager(schemeid, nil))
	require.NoError(t, client.Configuration.ParseFolder())
	require.Contains(t, client.Configuration.AttributeTypes, irma.NewAttributeTypeIdentifier("irma-demo.RU.studentCard.newAttribute"))

	// Check that the just created credential is still valid after the new attribute has been added
	_, status, err := serverResult.Signature.Verify(client.Configuration, nil)
	require.NoError(t, err)
	require.Equal(t, irma.ProofStatusValid, status)
}

func TestRequestorDisclosureSession(t *testing.T) {
	id := irma.NewAttributeTypeIdentifier("irma-demo.RU.studentCard.studentID")
	request := irma.NewDisclosureRequest(id)
	for _, opt := range []sessionOption{0, sessionOptionRetryPost} {
		serverResult := testRequestorDisclosure(t, request, opt)
		require.Len(t, serverResult.Disclosed, 1)
		require.Equal(t, id, serverResult.Disclosed[0][0].Identifier)
		require.Equal(t, "456", serverResult.Disclosed[0][0].Value["en"])
	}
}

func TestRequestorDisclosureMultipleAttrs(t *testing.T) {
	request := irma.NewDisclosureRequest(
		irma.NewAttributeTypeIdentifier("irma-demo.RU.studentCard.studentID"),
		irma.NewAttributeTypeIdentifier("irma-demo.RU.studentCard.level"),
	)
	serverResult := testRequestorDisclosure(t, request)
	require.Len(t, serverResult.Disclosed, 2)
}

func testRequestorDisclosure(t *testing.T, request *irma.DisclosureRequest, options ...sessionOption) *server.SessionResult {
	serverResult := requestorSessionHelper(t, request, nil, options...)
	require.Nil(t, serverResult.Err)
	require.Equal(t, irma.ProofStatusValid, serverResult.ProofStatus)
	return serverResult.SessionResult
}

func TestRequestorIssuanceSession(t *testing.T) {
	testRequestorIssuance(t, false, nil)
}

func TestRequestorCombinedSessionMultipleAttributes(t *testing.T) {
	var ir irma.IssuanceRequest
	require.NoError(t, irma.UnmarshalValidate([]byte(`{
		"type":"issuing",
		"credentials": [
			{
				"credential":"irma-demo.MijnOverheid.root",
				"attributes" : {
					"BSN":"12345"
				}
			}
		],
		"disclose" : [
			{
				"label":"Initialen",
				"attributes":["irma-demo.RU.studentCard.studentCardNumber"]
			},
			{
				"label":"Achternaam",
				"attributes" : ["irma-demo.RU.studentCard.studentID"]
			},
			{
				"label":"Geboortedatum",
				"attributes":["irma-demo.RU.studentCard.university"]
			}
		]
	}`), &ir))

	require.Equal(t, server.StatusDone, requestorSessionHelper(t, &ir, nil).Status)
}

func testRequestorIssuance(t *testing.T, keyshare bool, client *irmaclient.Client) {
	attrid := irma.NewAttributeTypeIdentifier("irma-demo.RU.studentCard.studentID")
	request := irma.NewIssuanceRequest([]*irma.CredentialRequest{{
		CredentialTypeID: irma.NewCredentialTypeIdentifier("irma-demo.RU.studentCard"),
		Attributes: map[string]string{
			"university":        "Radboud",
			"studentCardNumber": "31415927",
			"studentID":         "s1234567",
			"level":             "42",
		},
	}, {
		CredentialTypeID: irma.NewCredentialTypeIdentifier("irma-demo.MijnOverheid.root"),
		Attributes: map[string]string{
			"BSN": "299792458",
		},
	}}, attrid)
	if keyshare {
		request.Credentials = append(request.Credentials, &irma.CredentialRequest{
			CredentialTypeID: irma.NewCredentialTypeIdentifier("test.test.mijnirma"),
			Attributes:       map[string]string{"email": "testusername"},
		})
	}

	result := requestorSessionHelper(t, request, client)
	require.Nil(t, result.Err)
	require.Equal(t, irma.ProofStatusValid, result.ProofStatus)
	require.NotEmpty(t, result.Disclosed)
	require.Equal(t, attrid, result.Disclosed[0][0].Identifier)
	require.Equal(t, "456", result.Disclosed[0][0].Value["en"])
}

func TestConDisCon(t *testing.T) {
	client, _ := parseStorage(t)
	ir := getMultipleIssuanceRequest()
	ir.Credentials = append(ir.Credentials, &irma.CredentialRequest{
		Validity:         ir.Credentials[0].Validity,
		CredentialTypeID: irma.NewCredentialTypeIdentifier("irma-demo.MijnOverheid.fullName"),
		Attributes: map[string]string{
			"firstnames": "Jan Hendrik",
			"firstname":  "Jan",
			"familyname": "Klaassen",
			"prefix":     "van",
		},
	})
	requestorSessionHelper(t, ir, client)

	dr := irma.NewDisclosureRequest()
	dr.Disclose = irma.AttributeConDisCon{
		irma.AttributeDisCon{
			irma.AttributeCon{
				irma.NewAttributeRequest("irma-demo.MijnOverheid.root.BSN"),
				irma.NewAttributeRequest("irma-demo.MijnOverheid.fullName.firstname"),
				irma.NewAttributeRequest("irma-demo.MijnOverheid.fullName.familyname"),
			},
			irma.AttributeCon{
				irma.NewAttributeRequest("irma-demo.RU.studentCard.studentID"),
				irma.NewAttributeRequest("irma-demo.RU.studentCard.university"),
			},
		},
		//irma.AttributeDisCon{
		//	irma.AttributeCon{
		//		irma.NewAttributeRequest("irma-demo.MijnOverheid.fullName.firstname"),
		//		irma.NewAttributeRequest("irma-demo.MijnOverheid.fullName.familyname"),
		//	},
		//},
	}

	requestorSessionHelper(t, dr, client)
}

func TestOptionalDisclosure(t *testing.T) {
	client, _ := parseStorage(t)
	university := irma.NewAttributeTypeIdentifier("irma-demo.RU.studentCard.university")
	studentid := irma.NewAttributeTypeIdentifier("irma-demo.RU.studentCard.studentID")

	radboud := "Radboud"
	attrs1 := irma.AttributeConDisCon{
		irma.AttributeDisCon{ // Including one non-optional disjunction is required in disclosure and signature sessions
			irma.AttributeCon{irma.AttributeRequest{Type: university}},
		},
		irma.AttributeDisCon{
			irma.AttributeCon{},
			irma.AttributeCon{irma.AttributeRequest{Type: studentid}},
		},
	}
	disclosed1 := [][]*irma.DisclosedAttribute{
		{
			{
				RawValue:     &radboud,
				Value:        map[string]string{"": radboud, "en": radboud, "nl": radboud},
				Identifier:   irma.NewAttributeTypeIdentifier("irma-demo.RU.studentCard.university"),
				Status:       irma.AttributeProofStatusPresent,
				IssuanceTime: irma.Timestamp(client.Attributes(university.CredentialTypeIdentifier(), 0).SigningDate()),
			},
		},
		{},
	}
	attrs2 := irma.AttributeConDisCon{ // In issuance sessions, it is allowed that all disjunctions are optional
		irma.AttributeDisCon{
			irma.AttributeCon{},
			irma.AttributeCon{irma.AttributeRequest{Type: studentid}},
		},
	}
	disclosed2 := [][]*irma.DisclosedAttribute{{}}

	tests := []struct {
		request   irma.SessionRequest
		attrs     irma.AttributeConDisCon
		disclosed [][]*irma.DisclosedAttribute
	}{
		{irma.NewDisclosureRequest(), attrs1, disclosed1},
		{irma.NewSignatureRequest("message"), attrs1, disclosed1},
		{getIssuanceRequest(true), attrs1, disclosed1},
		{getIssuanceRequest(true), attrs2, disclosed2},
	}

	for _, args := range tests {
		args.request.Disclosure().Disclose = args.attrs

		// TestHandler always prefers the first option when given any choice, so it will not disclose the optional attribute
		result := requestorSessionHelper(t, args.request, client)
		require.True(t, reflect.DeepEqual(args.disclosed, result.Disclosed))
	}
}

func editDB(t *testing.T, path string, keystore revocation.Keystore, enabled bool, f func(*revocation.DB)) {
	StopRevocationServer()
	db, err := revocation.LoadDB(path, keystore)
	require.NoError(t, err)
	require.True(t, !enabled || db.Enabled())
	f(db)
	require.NoError(t, db.Close())
	StartRevocationServer(t)
}

func revocationSession(t *testing.T, client *irmaclient.Client, options ...sessionOption) *requestorSessionResult {
	attr := irma.NewAttributeTypeIdentifier("irma-demo.MijnOverheid.root.BSN")
	req := irma.NewDisclosureRequest(attr)
	req.Revocation = []irma.CredentialTypeIdentifier{attr.CredentialTypeIdentifier()}
	result := requestorSessionHelper(t, req, client, options...)
	require.Nil(t, result.Err)
	return result
}

func TestRevocation(t *testing.T) {
	// setup client, constants, and revocation key material
	defer test.ClearTestStorage(t)
	client, _ := parseStorage(t)
	iss := irma.NewIssuerIdentifier("irma-demo.MijnOverheid")
	cred := irma.NewCredentialTypeIdentifier("irma-demo.MijnOverheid.root")
	dbPath := filepath.Join(testdata, "tmp", "issuer", cred.String())
	keystore := client.Configuration.RevocationKeystore(iss)
	sk, err := client.Configuration.PrivateKey(iss)
	require.NoError(t, err)
	revsk, err := sk.RevocationKey()
	require.NoError(t, err)

	// enable revocation for our credential type by creating and saving an initial accumulator
	StartRevocationServer(t)
	editDB(t, dbPath, keystore, false, func(db *revocation.DB) {
		require.NoError(t, db.EnableRevocation(revsk))
	})

	// issue MijnOverheid.root instance with revocation enabled
	request := irma.NewIssuanceRequest([]*irma.CredentialRequest{{
		RevocationKey:    "12345", // once revocation is required for a credential type, this key is required
		CredentialTypeID: irma.NewCredentialTypeIdentifier("irma-demo.MijnOverheid.root"),
		Attributes: map[string]string{
			"BSN": "299792458",
		},
	}})
	result := requestorSessionHelper(t, request, client)
	require.Nil(t, result.Err)

	// perform disclosure session with nonrevocation proof
	result = revocationSession(t, client)
	require.Equal(t, irma.ProofStatusValid, result.ProofStatus)
	require.NotEmpty(t, result.Disclosed)

	// revoke fake other credential
	e, err := rand.Prime(rand.Reader, 207)
	require.NoError(t, err)
	editDB(t, dbPath, keystore, true, func(db *revocation.DB) {
		require.NoError(t, db.AddIssuanceRecord(&revocation.IssuanceRecord{
			Key:  "fake",
			Attr: big.Convert(e),
		}))
		require.NoError(t, db.Revoke(revsk, []byte("fake")))
	})

	// perform another disclosure session with nonrevocation proof
	// client updates its witness to the new accumulator first
	result = revocationSession(t, client)
	require.Equal(t, irma.ProofStatusValid, result.ProofStatus)
	require.NotEmpty(t, result.Disclosed)

	// revoke our credential
	editDB(t, dbPath, keystore, true, func(db *revocation.DB) {
		require.NoError(t, db.Revoke(revsk, []byte("12345")))
	})

	// try to perform session with revoked credential
	// client notices that is credential is revoked and aborts
	result = revocationSession(t, client, sessionOptionIgnoreClientError)
	require.Equal(t, result.Status, server.StatusCancelled)
}
