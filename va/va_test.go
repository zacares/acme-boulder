package va

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/jmhodges/clock"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"

	"github.com/letsencrypt/boulder/bdns"
	"github.com/letsencrypt/boulder/core"
	corepb "github.com/letsencrypt/boulder/core/proto"
	"github.com/letsencrypt/boulder/features"
	"github.com/letsencrypt/boulder/identifier"
	blog "github.com/letsencrypt/boulder/log"
	"github.com/letsencrypt/boulder/metrics"
	"github.com/letsencrypt/boulder/probs"
	"github.com/letsencrypt/boulder/test"
	vapb "github.com/letsencrypt/boulder/va/proto"
)

var expectedToken = "LoqXcYV8q5ONbJQxbmR7SCTNo3tiAXDfowyjxAjEuX0"
var expectedThumbprint = "9jg46WB3rR_AHD-EBXdN7cBkH1WOu0tA3M9fm21mqTI"
var expectedKeyAuthorization = ka(expectedToken)

func ka(token string) string {
	return token + "." + expectedThumbprint
}

func bigIntFromB64(b64 string) *big.Int {
	bytes, _ := base64.URLEncoding.DecodeString(b64)
	x := big.NewInt(0)
	x.SetBytes(bytes)
	return x
}

func intFromB64(b64 string) int {
	return int(bigIntFromB64(b64).Int64())
}

var n = bigIntFromB64("n4EPtAOCc9AlkeQHPzHStgAbgs7bTZLwUBZdR8_KuKPEHLd4rHVTeT-O-XV2jRojdNhxJWTDvNd7nqQ0VEiZQHz_AJmSCpMaJMRBSFKrKb2wqVwGU_NsYOYL-QtiWN2lbzcEe6XC0dApr5ydQLrHqkHHig3RBordaZ6Aj-oBHqFEHYpPe7Tpe-OfVfHd1E6cS6M1FZcD1NNLYD5lFHpPI9bTwJlsde3uhGqC0ZCuEHg8lhzwOHrtIQbS0FVbb9k3-tVTU4fg_3L_vniUFAKwuCLqKnS2BYwdq_mzSnbLY7h_qixoR7jig3__kRhuaxwUkRz5iaiQkqgc5gHdrNP5zw==")
var e = intFromB64("AQAB")
var d = bigIntFromB64("bWUC9B-EFRIo8kpGfh0ZuyGPvMNKvYWNtB_ikiH9k20eT-O1q_I78eiZkpXxXQ0UTEs2LsNRS-8uJbvQ-A1irkwMSMkK1J3XTGgdrhCku9gRldY7sNA_AKZGh-Q661_42rINLRCe8W-nZ34ui_qOfkLnK9QWDDqpaIsA-bMwWWSDFu2MUBYwkHTMEzLYGqOe04noqeq1hExBTHBOBdkMXiuFhUq1BU6l-DqEiWxqg82sXt2h-LMnT3046AOYJoRioz75tSUQfGCshWTBnP5uDjd18kKhyv07lhfSJdrPdM5Plyl21hsFf4L_mHCuoFau7gdsPfHPxxjVOcOpBrQzwQ==")
var p = bigIntFromB64("uKE2dh-cTf6ERF4k4e_jy78GfPYUIaUyoSSJuBzp3Cubk3OCqs6grT8bR_cu0Dm1MZwWmtdqDyI95HrUeq3MP15vMMON8lHTeZu2lmKvwqW7anV5UzhM1iZ7z4yMkuUwFWoBvyY898EXvRD-hdqRxHlSqAZ192zB3pVFJ0s7pFc=")
var q = bigIntFromB64("uKE2dh-cTf6ERF4k4e_jy78GfPYUIaUyoSSJuBzp3Cubk3OCqs6grT8bR_cu0Dm1MZwWmtdqDyI95HrUeq3MP15vMMON8lHTeZu2lmKvwqW7anV5UzhM1iZ7z4yMkuUwFWoBvyY898EXvRD-hdqRxHlSqAZ192zB3pVFJ0s7pFc=")

var TheKey = rsa.PrivateKey{
	PublicKey: rsa.PublicKey{N: n, E: e},
	D:         d,
	Primes:    []*big.Int{p, q},
}

var accountKey = &jose.JSONWebKey{Key: TheKey.Public()}

// Return an ACME DNS identifier for the given hostname
func dnsi(hostname string) identifier.ACMEIdentifier {
	return identifier.NewDNS(hostname)
}

var ctx context.Context

func TestMain(m *testing.M) {
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Minute)
	ret := m.Run()
	cancel()
	os.Exit(ret)
}

var accountURIPrefixes = []string{"http://boulder.service.consul:4000/acme/reg/"}

func createPerformValidationRequest(domain string, challengeType core.AcmeChallenge) *vapb.PerformValidationRequest {
	return &vapb.PerformValidationRequest{
		DnsName: domain,
		Challenge: &corepb.Challenge{
			Type:              string(challengeType),
			Status:            string(core.StatusPending),
			Token:             expectedToken,
			Validationrecords: nil,
		},
		Authz: &vapb.AuthzMeta{
			Id:    "",
			RegID: 1,
		},
		ExpectedKeyAuthorization: expectedKeyAuthorization,
	}
}

// setup returns an in-memory VA and a mock logger. The default resolver client
// is MockClient{}, but can be overridden.
func setup(srv *httptest.Server, maxRemoteFailures int, userAgent string, remoteVAs []RemoteVA, mockDNSClientOverride bdns.Client) (*ValidationAuthorityImpl, *blog.Mock) {
	features.Reset()
	fc := clock.NewFake()

	logger := blog.NewMock()

	if userAgent == "" {
		userAgent = "user agent 1.0"
	}

	va, err := NewValidationAuthorityImpl(
		&bdns.MockClient{Log: logger},
		nil,
		maxRemoteFailures,
		userAgent,
		"letsencrypt.org",
		metrics.NoopRegisterer,
		fc,
		logger,
		accountURIPrefixes,
		PrimaryPerspective,
		"",
	)

	if mockDNSClientOverride != nil {
		va.dnsClient = mockDNSClientOverride
	}

	// Adjusting industry regulated ACME challenge port settings is fine during
	// testing
	if srv != nil {
		port := getPort(srv)
		va.httpPort = port
		va.tlsPort = port
	}

	if err != nil {
		panic(fmt.Sprintf("Failed to create validation authority: %v", err))
	}
	if remoteVAs != nil {
		va.remoteVAs = remoteVAs
	}
	return va, logger
}

func setupRemote(srv *httptest.Server, userAgent string, mockDNSClientOverride bdns.Client) (RemoteClients, *blog.Mock) { //nolint: unparam
	rva, log := setup(srv, 0, userAgent, nil, mockDNSClientOverride)

	return RemoteClients{VAClient: &inMemVA{*rva}, CAAClient: &inMemVA{*rva}}, log
}

type multiSrv struct {
	*httptest.Server

	mu         sync.Mutex
	allowedUAs map[string]bool
}

func (s *multiSrv) setAllowedUAs(allowedUAs map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allowedUAs = allowedUAs
}

const slowRemoteSleepMillis = 1000

func httpMultiSrv(t *testing.T, token string, allowedUAs map[string]bool) *multiSrv {
	t.Helper()
	m := http.NewServeMux()

	server := httptest.NewUnstartedServer(m)
	ms := &multiSrv{server, sync.Mutex{}, allowedUAs}

	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() == "slow remote" {
			time.Sleep(slowRemoteSleepMillis)
		}
		ms.mu.Lock()
		defer ms.mu.Unlock()
		if ms.allowedUAs[r.UserAgent()] {
			ch := core.Challenge{Token: token}
			keyAuthz, _ := ch.ExpectedKeyAuthorization(accountKey)
			fmt.Fprint(w, keyAuthz, "\n\r \t")
		} else {
			fmt.Fprint(w, "???")
		}
	})

	ms.Start()
	return ms
}

// canceledVA is a mock that always returns context.Canceled for
// PerformValidation calls
type canceledVA struct{}

func (v canceledVA) PerformValidation(_ context.Context, _ *vapb.PerformValidationRequest, _ ...grpc.CallOption) (*vapb.ValidationResult, error) {
	return nil, context.Canceled
}

func (v canceledVA) IsCAAValid(_ context.Context, _ *vapb.IsCAAValidRequest, _ ...grpc.CallOption) (*vapb.IsCAAValidResponse, error) {
	return nil, context.Canceled
}

func (v canceledVA) ValidateChallenge(_ context.Context, _ *vapb.ValidationRequest, _ ...grpc.CallOption) (*vapb.ValidationResult, error) {
	return nil, context.Canceled
}

func (v canceledVA) CheckCAA(_ context.Context, _ *vapb.CheckCAARequest, _ ...grpc.CallOption) (*vapb.CheckCAAResult, error) {
	return nil, context.Canceled
}

// brokenRemoteVA is a mock for the VAClient and CAAClient interfaces that always return
// errors.
type brokenRemoteVA struct{}

// errBrokenRemoteVA is the error returned by a brokenRemoteVA's
// PerformValidation and IsSafeDomain functions.
var errBrokenRemoteVA = errors.New("brokenRemoteVA is broken")

// PerformValidation returns errBrokenRemoteVA unconditionally
func (b brokenRemoteVA) PerformValidation(_ context.Context, _ *vapb.PerformValidationRequest, _ ...grpc.CallOption) (*vapb.ValidationResult, error) {
	return nil, errBrokenRemoteVA
}

func (b brokenRemoteVA) IsCAAValid(_ context.Context, _ *vapb.IsCAAValidRequest, _ ...grpc.CallOption) (*vapb.IsCAAValidResponse, error) {
	return nil, errBrokenRemoteVA
}

func (b brokenRemoteVA) ValidateChallenge(_ context.Context, _ *vapb.ValidationRequest, _ ...grpc.CallOption) (*vapb.ValidationResult, error) {
	return nil, errBrokenRemoteVA
}

func (b brokenRemoteVA) CheckCAA(_ context.Context, _ *vapb.CheckCAARequest, _ ...grpc.CallOption) (*vapb.CheckCAAResult, error) {
	return nil, errBrokenRemoteVA
}

// inMemVA is a wrapper which fulfills the VAClient and CAAClient
// interfaces, but then forwards requests directly to its inner
// ValidationAuthorityImpl rather than over the network. This lets a local
// in-memory mock VA act like a remote VA.
type inMemVA struct {
	rva ValidationAuthorityImpl
}

func (inmem inMemVA) PerformValidation(ctx context.Context, req *vapb.PerformValidationRequest, _ ...grpc.CallOption) (*vapb.ValidationResult, error) {
	return inmem.rva.PerformValidation(ctx, req)
}

func (inmem inMemVA) IsCAAValid(ctx context.Context, req *vapb.IsCAAValidRequest, _ ...grpc.CallOption) (*vapb.IsCAAValidResponse, error) {
	return inmem.rva.IsCAAValid(ctx, req)
}

func (inmem inMemVA) ValidateChallenge(_ context.Context, req *vapb.ValidationRequest, _ ...grpc.CallOption) (*vapb.ValidationResult, error) {
	return inmem.rva.ValidateChallenge(ctx, req)
}

func (inmem inMemVA) CheckCAA(_ context.Context, req *vapb.CheckCAARequest, _ ...grpc.CallOption) (*vapb.CheckCAAResult, error) {
	return inmem.rva.CheckCAA(ctx, req)
}

func TestValidateMalformedChallenge(t *testing.T) {
	va, _ := setup(nil, 0, "", nil, nil)

	_, err := va.validateChallenge(ctx, dnsi("example.com"), "fake-type-01", expectedToken, expectedKeyAuthorization)

	prob := detailedError(err)
	test.AssertEquals(t, prob.Type, probs.MalformedProblem)
}

func TestPerformValidationInvalid(t *testing.T) {
	va, _ := setup(nil, 0, "", nil, nil)

	req := createPerformValidationRequest("foo.com", core.ChallengeTypeDNS01)
	res, _ := va.PerformValidation(context.Background(), req)
	test.Assert(t, res.Problems != nil, "validation succeeded")

	test.AssertMetricWithLabelsEquals(t, va.metrics.validationTime, prometheus.Labels{
		"type":         "dns-01",
		"result":       "invalid",
		"problem_type": "unauthorized",
	}, 1)
}

func TestInternalErrorLogged(t *testing.T) {
	va, mockLog := setup(nil, 0, "", nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	req := createPerformValidationRequest("nonexistent.com", core.ChallengeTypeHTTP01)
	_, err := va.PerformValidation(ctx, req)
	test.AssertNotError(t, err, "failed validation should not be an error")
	matchingLogs := mockLog.GetAllMatching(
		`Validation result JSON=.*"InternalError":"127.0.0.1: Get.*nonexistent.com/\.well-known.*: context deadline exceeded`)
	test.AssertEquals(t, len(matchingLogs), 1)
}

func TestPerformValidationValid(t *testing.T) {
	va, mockLog := setup(nil, 0, "", nil, nil)

	// create a challenge with well known token
	req := createPerformValidationRequest("good-dns01.com", core.ChallengeTypeDNS01)
	res, _ := va.PerformValidation(context.Background(), req)
	test.Assert(t, res.Problems == nil, fmt.Sprintf("validation failed: %#v", res.Problems))

	test.AssertMetricWithLabelsEquals(t, va.metrics.validationTime, prometheus.Labels{
		"type":         "dns-01",
		"result":       "valid",
		"problem_type": "",
	}, 1)
	resultLog := mockLog.GetAllMatching(`Validation result`)
	if len(resultLog) != 1 {
		t.Fatalf("Wrong number of matching lines for 'Validation result'")
	}
	if !strings.Contains(resultLog[0], `"Hostname":"good-dns01.com"`) {
		t.Error("PerformValidation didn't log validation hostname.")
	}
}

// TestPerformValidationWildcard tests that the VA properly strips the `*.`
// prefix from a wildcard name provided to the PerformValidation function.
func TestPerformValidationWildcard(t *testing.T) {
	va, mockLog := setup(nil, 0, "", nil, nil)

	// create a challenge with well known token
	req := createPerformValidationRequest("*.good-dns01.com", core.ChallengeTypeDNS01)
	// perform a validation for a wildcard name
	res, _ := va.PerformValidation(context.Background(), req)
	test.Assert(t, res.Problems == nil, fmt.Sprintf("validation failed: %#v", res.Problems))

	test.AssertMetricWithLabelsEquals(t, va.metrics.validationTime, prometheus.Labels{
		"type":         "dns-01",
		"result":       "valid",
		"problem_type": "",
	}, 1)
	resultLog := mockLog.GetAllMatching(`Validation result`)
	if len(resultLog) != 1 {
		t.Fatalf("Wrong number of matching lines for 'Validation result'")
	}

	// We expect that the top level Hostname reflect the wildcard name
	if !strings.Contains(resultLog[0], `"Hostname":"*.good-dns01.com"`) {
		t.Errorf("PerformValidation didn't log correct validation hostname.")
	}
	// We expect that the ValidationRecord contain the correct non-wildcard
	// hostname that was validated
	if !strings.Contains(resultLog[0], `"hostname":"good-dns01.com"`) {
		t.Errorf("PerformValidation didn't log correct validation record hostname.")
	}
}

func TestDCVAndCAASequencing(t *testing.T) {
	va, mockLog := setup(nil, 0, "", nil, nil)

	// When validation succeeds, CAA should be checked.
	mockLog.Clear()
	req := createPerformValidationRequest("good-dns01.com", core.ChallengeTypeDNS01)
	res, err := va.PerformValidation(context.Background(), req)
	test.AssertNotError(t, err, "performing validation")
	test.Assert(t, res.Problems == nil, fmt.Sprintf("validation failed: %#v", res.Problems))
	caaLog := mockLog.GetAllMatching(`Checked CAA records for`)
	test.AssertEquals(t, len(caaLog), 1)

	// When validation fails, CAA should be skipped.
	mockLog.Clear()
	req = createPerformValidationRequest("bad-dns01.com", core.ChallengeTypeDNS01)
	res, err = va.PerformValidation(context.Background(), req)
	test.AssertNotError(t, err, "performing validation")
	test.Assert(t, res.Problems != nil, "validation succeeded")
	caaLog = mockLog.GetAllMatching(`Checked CAA records for`)
	test.AssertEquals(t, len(caaLog), 0)
}

func TestMultiVA(t *testing.T) {
	// Create a new challenge to use for the httpSrv
	req := createPerformValidationRequest("localhost", core.ChallengeTypeHTTP01)

	const (
		remoteUA1 = "remote 1"
		remoteUA2 = "remote 2"
		localUA   = "local 1"
	)
	allowedUAs := map[string]bool{
		localUA:   true,
		remoteUA1: true,
		remoteUA2: true,
	}

	// Create an IPv4 test server
	ms := httpMultiSrv(t, expectedToken, allowedUAs)
	defer ms.Close()

	remoteVA1, _ := setupRemote(ms.Server, remoteUA1, nil)
	remoteVA2, _ := setupRemote(ms.Server, remoteUA2, nil)
	remoteVAs := []RemoteVA{
		{remoteVA1, remoteUA1},
		{remoteVA2, remoteUA2},
	}
	brokenVA := RemoteClients{
		VAClient:  brokenRemoteVA{},
		CAAClient: brokenRemoteVA{},
	}
	cancelledVA := RemoteClients{
		VAClient:  canceledVA{},
		CAAClient: canceledVA{},
	}

	unauthorized := probs.Unauthorized(fmt.Sprintf(
		`The key authorization file from the server did not match this challenge. Expected %q (got "???")`,
		expectedKeyAuthorization))
	expectedInternalErrLine := fmt.Sprintf(
		`ERR: \[AUDIT\] Remote VA "broken".PerformValidation failed: %s`,
		errBrokenRemoteVA.Error())
	testCases := []struct {
		Name         string
		RemoteVAs    []RemoteVA
		AllowedUAs   map[string]bool
		ExpectedProb *probs.ProblemDetails
		ExpectedLog  string
	}{
		{
			// With local and both remote VAs working there should be no problem.
			Name:       "Local and remote VAs OK",
			RemoteVAs:  remoteVAs,
			AllowedUAs: allowedUAs,
		},
		{
			// If the local VA fails everything should fail
			Name:         "Local VA bad, remote VAs OK",
			RemoteVAs:    remoteVAs,
			AllowedUAs:   map[string]bool{remoteUA1: true, remoteUA2: true},
			ExpectedProb: unauthorized,
		},
		{
			// If a remote VA fails with an internal err it should fail
			Name: "Local VA ok, remote VA internal err",
			RemoteVAs: []RemoteVA{
				{remoteVA1, remoteUA1},
				{brokenVA, "broken"},
			},
			AllowedUAs:   allowedUAs,
			ExpectedProb: probs.ServerInternal("During secondary validation: Remote PerformValidation RPC failed"),
			// The real failure cause should be logged
			ExpectedLog: expectedInternalErrLine,
		},
		{
			// With only one working remote VA there should be a validation failure
			Name:       "Local VA and one remote VA OK",
			RemoteVAs:  remoteVAs,
			AllowedUAs: map[string]bool{localUA: true, remoteUA2: true},
			ExpectedProb: probs.Unauthorized(fmt.Sprintf(
				`During secondary validation: The key authorization file from the server did not match this challenge. Expected %q (got "???")`,
				expectedKeyAuthorization)),
		},
		{
			// Any remote VA cancellations are a problem.
			Name: "Local VA and one remote VA OK, one cancelled VA",
			RemoteVAs: []RemoteVA{
				{remoteVA1, remoteUA1},
				{cancelledVA, remoteUA2},
			},
			AllowedUAs:   allowedUAs,
			ExpectedProb: probs.ServerInternal("During secondary validation: Remote PerformValidation RPC canceled"),
		},
		{
			// Any remote VA cancellations are a problem.
			Name: "Local VA OK, two cancelled remote VAs",
			RemoteVAs: []RemoteVA{
				{cancelledVA, remoteUA1},
				{cancelledVA, remoteUA2},
			},
			AllowedUAs:   allowedUAs,
			ExpectedProb: probs.ServerInternal("During secondary validation: Remote PerformValidation RPC canceled"),
		},
		{
			// With the local and remote VAs seeing diff problems, we expect a problem.
			Name:       "Local and remote VA differential, full results, enforce multi VA",
			RemoteVAs:  remoteVAs,
			AllowedUAs: map[string]bool{localUA: true},
			ExpectedProb: probs.Unauthorized(fmt.Sprintf(
				`During secondary validation: The key authorization file from the server did not match this challenge. Expected %q (got "???")`,
				expectedKeyAuthorization)),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			// Configure the test server with the testcase allowed UAs.
			ms.setAllowedUAs(tc.AllowedUAs)

			// Configure a primary VA with testcase remote VAs.
			localVA, mockLog := setup(ms.Server, 0, localUA, tc.RemoteVAs, nil)

			// Perform all validations
			res, _ := localVA.PerformValidation(ctx, req)
			if res.Problems == nil && tc.ExpectedProb != nil {
				t.Errorf("expected prob %v, got nil", tc.ExpectedProb)
			} else if res.Problems != nil && tc.ExpectedProb == nil {
				t.Errorf("expected no prob, got %v", res.Problems)
			} else if res.Problems != nil && tc.ExpectedProb != nil {
				// That result should match expected.
				test.AssertEquals(t, res.Problems.ProblemType, string(tc.ExpectedProb.Type))
				test.AssertEquals(t, res.Problems.Detail, tc.ExpectedProb.Detail)
			}

			if tc.ExpectedLog != "" {
				lines := mockLog.GetAllMatching(tc.ExpectedLog)
				if len(lines) != 1 {
					t.Fatalf("Got log %v; expected %q", mockLog.GetAll(), tc.ExpectedLog)
				}
			}
		})
	}
}

func TestMultiVAEarlyReturn(t *testing.T) {
	const (
		remoteUA1 = "remote 1"
		remoteUA2 = "slow remote"
		localUA   = "local 1"
	)
	allowedUAs := map[string]bool{
		localUA:   true,
		remoteUA1: false, // forbid UA 1 to provoke early return
		remoteUA2: true,
	}

	ms := httpMultiSrv(t, expectedToken, allowedUAs)
	defer ms.Close()

	remoteVA1, _ := setupRemote(ms.Server, remoteUA1, nil)
	remoteVA2, _ := setupRemote(ms.Server, remoteUA2, nil)

	remoteVAs := []RemoteVA{
		{remoteVA1, remoteUA1},
		{remoteVA2, remoteUA2},
	}

	// Create a local test VA with the two remote VAs
	localVA, _ := setup(ms.Server, 0, localUA, remoteVAs, nil)

	// Perform all validations
	start := time.Now()
	req := createPerformValidationRequest("localhost", core.ChallengeTypeHTTP01)
	res, _ := localVA.PerformValidation(ctx, req)

	// It should always fail
	if res.Problems == nil {
		t.Error("expected prob from PerformValidation, got nil")
	}

	elapsed := time.Since(start).Round(time.Millisecond).Milliseconds()

	// The slow UA should sleep for `slowRemoteSleepMillis`. But the first remote
	// VA should fail quickly and the early-return code should cause the overall
	// overall validation to return a prob quickly (i.e. in less than half of
	// `slowRemoteSleepMillis`).
	if elapsed > slowRemoteSleepMillis/2 {
		t.Errorf(
			"Expected an early return from PerformValidation in < %d ms, took %d ms",
			slowRemoteSleepMillis/2, elapsed)
	}
}

func TestMultiVAPolicy(t *testing.T) {
	const (
		remoteUA1 = "remote 1"
		remoteUA2 = "remote 2"
		localUA   = "local 1"
	)
	// Forbid both remote UAs to ensure that multi-va fails
	allowedUAs := map[string]bool{
		localUA:   true,
		remoteUA1: false,
		remoteUA2: false,
	}

	ms := httpMultiSrv(t, expectedToken, allowedUAs)
	defer ms.Close()

	remoteVA1, _ := setupRemote(ms.Server, remoteUA1, nil)
	remoteVA2, _ := setupRemote(ms.Server, remoteUA2, nil)

	remoteVAs := []RemoteVA{
		{remoteVA1, remoteUA1},
		{remoteVA2, remoteUA2},
	}

	// Create a local test VA with the two remote VAs
	localVA, _ := setup(ms.Server, 0, localUA, remoteVAs, nil)

	// Perform validation for a domain not in the disabledDomains list
	req := createPerformValidationRequest("letsencrypt.org", core.ChallengeTypeHTTP01)
	res, _ := localVA.PerformValidation(ctx, req)
	// It should fail
	if res.Problems == nil {
		t.Error("expected prob from PerformValidation, got nil")
	}
}

func TestMultiVALogging(t *testing.T) {
	const (
		rva1UA  = "remote 1"
		rva2UA  = "remote 2"
		localUA = "local 1"
	)

	ms := httpMultiSrv(t, expectedToken, map[string]bool{localUA: true, rva1UA: true, rva2UA: true})
	defer ms.Close()

	rva1, _ := setupRemote(ms.Server, rva1UA, nil)
	rva2, _ := setupRemote(ms.Server, rva2UA, nil)

	remoteVAs := []RemoteVA{
		{rva1, rva1UA},
		{rva2, rva2UA},
	}
	va, _ := setup(ms.Server, 0, localUA, remoteVAs, nil)
	req := createPerformValidationRequest("letsencrypt.org", core.ChallengeTypeHTTP01)
	res, err := va.PerformValidation(ctx, req)
	test.Assert(t, res.Problems == nil, fmt.Sprintf("validation failed with: %#v", res.Problems))
	test.AssertNotError(t, err, "performing validation")
}

func TestDetailedError(t *testing.T) {
	cases := []struct {
		err      error
		ip       net.IP
		expected string
	}{
		{
			err: ipError{
				ip: net.ParseIP("192.168.1.1"),
				err: &net.OpError{
					Op:  "dial",
					Net: "tcp",
					Err: &os.SyscallError{
						Syscall: "getsockopt",
						Err:     syscall.ECONNREFUSED,
					},
				},
			},
			expected: "192.168.1.1: Connection refused",
		},
		{
			err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: &os.SyscallError{
					Syscall: "getsockopt",
					Err:     syscall.ECONNREFUSED,
				},
			},
			expected: "Connection refused",
		},
		{
			err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: &os.SyscallError{
					Syscall: "getsockopt",
					Err:     syscall.ECONNRESET,
				},
			},
			ip:       nil,
			expected: "Connection reset by peer",
		},
	}
	for _, tc := range cases {
		actual := detailedError(tc.err).Detail
		if actual != tc.expected {
			t.Errorf("Wrong detail for %v. Got %q, expected %q", tc.err, actual, tc.expected)
		}
	}
}

func TestLogRemoteDifferentials(t *testing.T) {
	// Create some remote VAs
	remoteVA1, _ := setupRemote(nil, "remote 1", nil)
	remoteVA2, _ := setupRemote(nil, "remote 2", nil)
	remoteVA3, _ := setupRemote(nil, "remote 3", nil)
	remoteVAs := []RemoteVA{
		{remoteVA1, "remote 1"},
		{remoteVA2, "remote 2"},
		{remoteVA3, "remote 3"},
	}

	// Set up a local VA that allows a max of 2 remote failures.
	localVA, mockLog := setup(nil, 2, "local 1", remoteVAs, nil)

	egProbA := probs.DNS("root DNS servers closed at 4:30pm")
	egProbB := probs.OrderNotReady("please take a number")

	testCases := []struct {
		name        string
		remoteProbs []*remoteVAResult
		expectedLog string
	}{
		{
			name: "all results equal (nil)",
			remoteProbs: []*remoteVAResult{
				{Problem: nil, VAHostname: "remoteA"},
				{Problem: nil, VAHostname: "remoteB"},
				{Problem: nil, VAHostname: "remoteC"},
			},
		},
		{
			name: "all results equal (not nil)",
			remoteProbs: []*remoteVAResult{
				{Problem: egProbA, VAHostname: "remoteA"},
				{Problem: egProbA, VAHostname: "remoteB"},
				{Problem: egProbA, VAHostname: "remoteC"},
			},
			expectedLog: `INFO: remoteVADifferentials JSON={"Domain":"example.com","AccountID":1999,"ChallengeType":"blorpus-01","RemoteSuccesses":0,"RemoteFailures":[{"VAHostname":"remoteA","Problem":{"type":"dns","detail":"root DNS servers closed at 4:30pm","status":400}},{"VAHostname":"remoteB","Problem":{"type":"dns","detail":"root DNS servers closed at 4:30pm","status":400}},{"VAHostname":"remoteC","Problem":{"type":"dns","detail":"root DNS servers closed at 4:30pm","status":400}}]}`,
		},
		{
			name: "differing results, some non-nil",
			remoteProbs: []*remoteVAResult{
				{Problem: nil, VAHostname: "remoteA"},
				{Problem: egProbB, VAHostname: "remoteB"},
				{Problem: nil, VAHostname: "remoteC"},
			},
			expectedLog: `INFO: remoteVADifferentials JSON={"Domain":"example.com","AccountID":1999,"ChallengeType":"blorpus-01","RemoteSuccesses":2,"RemoteFailures":[{"VAHostname":"remoteB","Problem":{"type":"orderNotReady","detail":"please take a number","status":403}}]}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mockLog.Clear()

			localVA.logRemoteResults(
				"example.com", 1999, "blorpus-01", tc.remoteProbs)

			lines := mockLog.GetAllMatching("remoteVADifferentials JSON=.*")
			if tc.expectedLog != "" {
				test.AssertEquals(t, len(lines), 1)
				test.AssertEquals(t, lines[0], tc.expectedLog)
			} else {
				test.AssertEquals(t, len(lines), 0)
			}
		})
	}
}

const (
	brokenDNS   = "broken-dns"
	hijackedDNS = "hijacked-dns"
)

func dnsClientForUA(ua string, log blog.Logger) bdns.Client {
	switch ua {
	case brokenDNS:
		return caaBrokenDNS{}
	case hijackedDNS:
		return caaHijackedDNS{}
	default:
		return &bdns.MockClient{Log: log}
	}
}

// setup returns an in-memory VA and a mock logger. The default resolver client
// is MockClient{}, but can be overridden.
func setupVA(srv *httptest.Server, ua string, rvas []RemoteVA, dnsClientMock bdns.Client) (*ValidationAuthorityImpl, *blog.Mock) { //nolint: unparam
	features.Reset()
	fc := clock.NewFake()

	mockLog := blog.NewMock()

	var dnsClient bdns.Client
	if dnsClientMock != nil {
		dnsClient = dnsClientMock
	} else {
		dnsClient = dnsClientForUA(ua, mockLog)
	}

	va, err := NewValidationAuthorityImpl(
		dnsClient,
		nil,
		0,
		ua,
		"letsencrypt.org",
		metrics.NoopRegisterer,
		fc,
		mockLog,
		accountURIPrefixes,
		PrimaryPerspective,
		"ARIN",
	)

	// Adjusting industry regulated ACME challenge port settings is fine during
	// testing
	if srv != nil {
		port := getPort(srv)
		va.httpPort = port
		va.tlsPort = port
	}

	if err != nil {
		panic(fmt.Sprintf("Failed to create validation authority: %v", err))
	}
	if rvas != nil {
		va.remoteVAs = rvas
	}
	return va, mockLog
}

type rvaConf struct {
	rir string
	ua  string
}

// setupRVAs returns a slice of RemoteVA instances for testing. confs is a slice
// of rir and user agent configurations for each RVA. mockDNSClient is optional,
// it allows the DNS client to be overridden. srv is optional, it allows for a
// test server to be specified.
func setupRVAs(confs []rvaConf, srv *httptest.Server) []RemoteVA {
	remoteVAs := make([]RemoteVA, 0, len(confs))
	for i, c := range confs {
		// Configure the remote VA.
		rva, _ := setupVA(srv, c.ua, nil, nil)
		rva.perspective = fmt.Sprintf("dc-%d-%s", i, c.rir)
		rva.rir = c.rir

		// Initialize the remote VA.
		remoteVAs = append(remoteVAs, RemoteVA{
			Address: fmt.Sprintf("dc-%d-%s", i, c.rir),
			RemoteClients: RemoteClients{
				VAClient:  &inMemVA{*rva},
				CAAClient: &inMemVA{*rva},
			},
		})
	}
	return remoteVAs
}

func createValidationRequest(domain string, challengeType core.AcmeChallenge) *vapb.ValidationRequest {
	return &vapb.ValidationRequest{
		Identifier: &corepb.Identifier{
			Type:  string(identifier.TypeDNS),
			Value: domain,
		},
		Challenge: &corepb.Challenge{
			Type:   string(challengeType),
			Status: string(core.StatusPending),
			Token:  expectedToken,
		},
		RegID:            1,
		AuthzID:          "1",
		KeyAuthorization: expectedKeyAuthorization,
	}
}

// parseValidationAuditLog extracts ... from JSON={ ... } in a ValidateChallenge
// audit log and returns it as a validateChallengeAuditLog struct.
func parseValidationAuditLog(t *testing.T, log []string) validateChallengeAuditLog {
	re := regexp.MustCompile(`JSON=\{.*\}`)
	var audit validateChallengeAuditLog
	for _, line := range log {
		match := re.FindString(line)
		if match != "" {
			jsonStr := match[len(`JSON=`):]
			if err := json.Unmarshal([]byte(jsonStr), &audit); err != nil {
				t.Fatalf("Failed to parse JSON: %v", err)
			}
			return audit
		}
	}
	t.Fatal("JSON not found in log")
	return audit
}

func TestValidateChallenge(t *testing.T) {
	t.Parallel()

	brokenRVA := RemoteVA{RemoteClients{VAClient: brokenRemoteVA{}}, "broken"}
	canceledRVA := RemoteVA{RemoteClients{VAClient: canceledVA{}}, "canceled"}

	testCases := []struct {
		name                          string
		identifier                    string
		challengeType                 core.AcmeChallenge
		rvas                          []rvaConf
		appendRemoteVAs               []RemoteVA
		primaryUA                     string
		ctxTimeout                    time.Duration
		expectError                   bool
		expectProbType                probs.ProblemType
		expectProbContains            string
		expectLogIdentifier           string
		expectValidationRecordDnsName string
		expectedMetricLabels          prometheus.Labels
	}{
		{
			name:                          "UnauthorizedProblem",
			identifier:                    "foo.com",
			challengeType:                 core.ChallengeTypeDNS01,
			rvas:                          []rvaConf{{"ARIN", pass}, {"RIPE", pass}, {"APNIC", pass}},
			primaryUA:                     pass,
			expectError:                   false,
			expectProbType:                probs.UnauthorizedProblem,
			expectProbContains:            "",
			expectLogIdentifier:           "foo.com",
			expectValidationRecordDnsName: "",
			expectedMetricLabels: prometheus.Labels{
				"operation":      opChallenge,
				"perspective":    PrimaryPerspective,
				"challenge_type": string(core.ChallengeTypeDNS01),
				"problem_type":   string(probs.UnauthorizedProblem),
				"result":         fail,
			},
		},
		{
			name:                          "ConnectionProblem",
			identifier:                    "nonexistent.com",
			challengeType:                 core.ChallengeTypeHTTP01,
			rvas:                          []rvaConf{{"ARIN", pass}, {"RIPE", pass}, {"APNIC", pass}},
			primaryUA:                     pass,
			ctxTimeout:                    1 * time.Millisecond,
			expectError:                   false,
			expectProbType:                probs.ConnectionProblem,
			expectProbContains:            "Timeout after connect",
			expectLogIdentifier:           "nonexistent.com",
			expectValidationRecordDnsName: "",
			expectedMetricLabels: prometheus.Labels{
				"operation":      opChallenge,
				"perspective":    PrimaryPerspective,
				"challenge_type": string(core.ChallengeTypeHTTP01),
				"problem_type":   string(probs.ConnectionProblem),
				"result":         fail,
			},
		},
		{
			name:                          "Valid",
			identifier:                    "good-dns01.com",
			challengeType:                 core.ChallengeTypeDNS01,
			rvas:                          []rvaConf{{"ARIN", pass}, {"RIPE", pass}, {"APNIC", pass}},
			primaryUA:                     pass,
			expectError:                   false,
			expectProbType:                "",
			expectLogIdentifier:           "good-dns01.com",
			expectValidationRecordDnsName: "good-dns01.com",
			expectedMetricLabels: prometheus.Labels{
				"operation":      opChallenge,
				"perspective":    PrimaryPerspective,
				"challenge_type": string(core.ChallengeTypeDNS01),
				"problem_type":   "",
				"result":         pass,
			},
		},
		{
			name:                          "ValidWithWildcard",
			identifier:                    "*.good-dns01.com",
			challengeType:                 core.ChallengeTypeDNS01,
			rvas:                          []rvaConf{{"ARIN", pass}, {"RIPE", pass}, {"APNIC", pass}},
			primaryUA:                     pass,
			expectError:                   false,
			expectProbType:                "",
			expectLogIdentifier:           "*.good-dns01.com",
			expectValidationRecordDnsName: "good-dns01.com",
			expectedMetricLabels: prometheus.Labels{
				"operation":      opChallenge,
				"perspective":    PrimaryPerspective,
				"challenge_type": string(core.ChallengeTypeDNS01),
				"problem_type":   "",
				"result":         pass,
			},
		},
		{
			name:                          "ValidWithBrokenRVA",
			identifier:                    "good-dns01.com",
			challengeType:                 core.ChallengeTypeDNS01,
			rvas:                          []rvaConf{{"ARIN", pass}, {"RIPE", pass}},
			appendRemoteVAs:               []RemoteVA{brokenRVA},
			primaryUA:                     pass,
			expectError:                   false,
			expectProbType:                "",
			expectLogIdentifier:           "good-dns01.com",
			expectValidationRecordDnsName: "good-dns01.com",
			expectedMetricLabels: prometheus.Labels{
				"operation":      opChallenge,
				"perspective":    PrimaryPerspective,
				"challenge_type": string(core.ChallengeTypeDNS01),
				"problem_type":   "",
				"result":         pass,
			},
		},
		{
			name:                          "ValidWithCanceledRVA",
			identifier:                    "good-dns01.com",
			challengeType:                 core.ChallengeTypeDNS01,
			rvas:                          []rvaConf{{"ARIN", pass}, {"RIPE", pass}},
			appendRemoteVAs:               []RemoteVA{canceledRVA},
			primaryUA:                     pass,
			expectError:                   false,
			expectProbType:                "",
			expectLogIdentifier:           "good-dns01.com",
			expectValidationRecordDnsName: "good-dns01.com",
			expectedMetricLabels: prometheus.Labels{
				"operation":      opChallenge,
				"perspective":    PrimaryPerspective,
				"challenge_type": string(core.ChallengeTypeDNS01),
				"problem_type":   "",
				"result":         pass,
			},
		},
		{
			name:                "InvalidWithTooManyBrokenRVAs",
			identifier:          "good-dns01.com",
			challengeType:       core.ChallengeTypeDNS01,
			rvas:                []rvaConf{{"ARIN", pass}},
			appendRemoteVAs:     []RemoteVA{brokenRVA, brokenRVA},
			primaryUA:           pass,
			expectError:         false,
			expectProbType:      probs.ServerInternalProblem,
			expectProbContains:  "During secondary domain validation: Secondary domain validation RPC failed",
			expectLogIdentifier: "good-dns01.com",
			expectedMetricLabels: prometheus.Labels{
				"operation":      opChallenge,
				"perspective":    PrimaryPerspective,
				"challenge_type": string(core.ChallengeTypeDNS01),
				"problem_type":   string(probs.ServerInternalProblem),
				"result":         fail,
			},
		},
		{
			name:                "InvalidWithTooManyCanceledRVAs",
			identifier:          "good-dns01.com",
			challengeType:       core.ChallengeTypeDNS01,
			rvas:                []rvaConf{{"ARIN", pass}},
			appendRemoteVAs:     []RemoteVA{canceledRVA, canceledRVA},
			primaryUA:           pass,
			expectError:         false,
			expectProbType:      probs.ServerInternalProblem,
			expectProbContains:  "During secondary domain validation: Secondary domain validation RPC canceled",
			expectLogIdentifier: "good-dns01.com",
			expectedMetricLabels: prometheus.Labels{
				"operation":      opChallenge,
				"perspective":    PrimaryPerspective,
				"challenge_type": string(core.ChallengeTypeDNS01),
				"problem_type":   string(probs.ServerInternalProblem),
				"result":         fail,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httpMultiSrv(t, expectedToken, map[string]bool{"pass": true, "fail": false})
			defer srv.Close()

			rvas := setupRVAs(tc.rvas, srv.Server)
			if len(tc.appendRemoteVAs) > 0 {
				rvas = append(rvas, tc.appendRemoteVAs...)
			}
			va, mockLog := setupVA(srv.Server, tc.primaryUA, rvas, nil)
			req := createValidationRequest(tc.identifier, tc.challengeType)

			ctx := context.Background()
			if tc.ctxTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(context.Background(), tc.ctxTimeout)
				defer cancel()
			}

			res, err := va.ValidateChallenge(ctx, req)
			if tc.expectError {
				test.AssertError(t, err, "Expected an error but got none")
			} else {
				test.AssertNotError(t, err, "ValidateChallenge failed, expected success")
			}

			resultLog := mockLog.GetAllMatching("Challenge validation result")
			test.AssertNotNil(t, resultLog, "ValidateChallenge didn't log validation result.")
			auditLog := parseValidationAuditLog(t, resultLog)
			if tc.expectLogIdentifier != "" {
				test.AssertEquals(t, tc.expectLogIdentifier, auditLog.Identifier)
			}
			if tc.expectValidationRecordDnsName != "" {
				if auditLog.Challenge.ValidationRecord == nil {
					t.Fatalf("Expected ValidationRecord in audit log but got none")
				}
				test.AssertEquals(t, tc.expectValidationRecordDnsName, auditLog.Challenge.ValidationRecord[0].DnsName)
			}

			if tc.expectProbType != "" {
				test.Assert(t, res.Problems != nil, "validation succeeded, expected failure")
				test.AssertEquals(t, string(tc.expectProbType), res.Problems.ProblemType)
				if tc.expectProbContains != "" {
					test.AssertContains(t, res.Problems.Detail, tc.expectProbContains)
				}
			} else {
				test.Assert(t, res.Problems == nil, fmt.Sprintf("validation failed unexpectedly: %#v", res.Problems))
			}
			test.AssertMetricWithLabelsEquals(t, va.metrics.validationLatency, tc.expectedMetricLabels, 1)
		})
	}
}

func TestValidateChallengeMPIC(t *testing.T) {
	t.Parallel()

	req := createValidationRequest("localhost", core.ChallengeTypeHTTP01)

	testCases := []struct {
		name               string
		primaryUA          string
		rvas               []rvaConf
		expectedProbType   probs.ProblemType
		expectLogContains  string
		expectQuorumResult string
		expectPassedRIRs   []string
	}{
		{
			// If the primary and all remote VAs pass, the validation will succeed.
			name:               "VA: pass, remote1(ARIN): pass, remote2(RIPE): pass, remote3(APNIC): pass",
			primaryUA:          pass,
			rvas:               []rvaConf{{"ARIN", pass}, {"RIPE", pass}, {"APNIC", pass}},
			expectedProbType:   "",
			expectLogContains:  `"Challenge":{"type":"http-01","status":"valid"`,
			expectQuorumResult: "3/3",
			expectPassedRIRs:   []string{"APNIC", "ARIN", "RIPE"},
		},
		{
			// If the primary passes and just one remote VA fails, the
			// validation will succeed.
			name:               "VA: pass, rva1(ARIN): pass, rva2(RIPE): pass, rva3(APNIC): fail",
			primaryUA:          pass,
			rvas:               []rvaConf{{"ARIN", pass}, {"RIPE", pass}, {"APNIC", fail}},
			expectedProbType:   "",
			expectLogContains:  `"Challenge":{"type":"http-01","status":"valid"`,
			expectQuorumResult: "2/3",
			expectPassedRIRs:   []string{"ARIN", "RIPE"},
		},
		{
			// If the primary passes and two remote VAs fail, the validation
			// will fail.
			name:               "VA: pass, rva1(ARIN): pass, rva2(RIPE): fail, rva3(APNIC): fail",
			primaryUA:          pass,
			rvas:               []rvaConf{{"ARIN", pass}, {"RIPE", fail}, {"APNIC", fail}},
			expectedProbType:   probs.UnauthorizedProblem,
			expectLogContains:  "During secondary domain validation: The key authorization file from the server did not match this challenge.",
			expectQuorumResult: "1/3",
			expectPassedRIRs:   []string{"ARIN"},
		},
		{
			// If the primary fails, the remote VAs will not be queried, and the
			// validation will fail.
			name:               "VA: fail, rva1(ARIN): pass, rva2(RIPE): pass, rva3(APNIC): pass",
			primaryUA:          fail,
			rvas:               []rvaConf{{"ARIN", pass}, {"RIPE", pass}, {"APNIC", pass}},
			expectedProbType:   probs.UnauthorizedProblem,
			expectLogContains:  "The key authorization file from the server did not match this challenge.",
			expectQuorumResult: "",
			expectPassedRIRs:   nil,
		},
		{
			// If the primary passes and all of the passing RVAs are from the
			// same RIR, the validation will fail and the error message will
			// indicate the problem.
			name:               "VA: pass, rva1(ARIN): pass, rva2(ARIN): pass, rva3(APNIC): fail",
			primaryUA:          pass,
			rvas:               []rvaConf{{"ARIN", pass}, {"ARIN", pass}, {"APNIC", fail}},
			expectedProbType:   probs.UnauthorizedProblem,
			expectLogContains:  "During secondary domain validation: The key authorization file from the server did not match this challenge.",
			expectQuorumResult: "2/3",
			expectPassedRIRs:   []string{"ARIN"},
		},
		{
			// If the primary passes and is configured with 6+ remote VAs, then
			// the validation can succeed with up to 2 remote VA failures and
			// successes from at least 2 distinct RIRs.
			name:      "VA: pass, rva1(ARIN): pass, rva2(APNIC): pass, rva3(ARIN): pass, rva4(ARIN): pass, rva5(ARIN): pass, rva6(ARIN): pass, rva7(ARIN): fail, rva8(ARIN): fail",
			primaryUA: pass,
			rvas: []rvaConf{
				{"ARIN", pass}, {"APNIC", pass}, {"ARIN", pass}, {"ARIN", pass}, {"ARIN", fail}, {"ARIN", fail},
			},
			expectedProbType:   "",
			expectLogContains:  `"Challenge":{"type":"http-01","status":"valid"`,
			expectQuorumResult: "4/6",
			expectPassedRIRs:   []string{"APNIC", "ARIN"},
		},
		{
			// If the primary passes and is configured with 6+ remote VAs which
			// return 3 or more failures, the validation will fail.
			name:      "VA: pass, rva1(ARIN): pass, rva2(APNIC): pass, rva3(ARIN): pass, rva4(ARIN): pass, rva5(ARIN): pass, rva6(ARIN): fail, rva7(ARIN): fail, rva8(ARIN): fail",
			primaryUA: pass,
			rvas: []rvaConf{
				{"ARIN", pass}, {"APNIC", pass}, {"ARIN", pass}, {"ARIN", pass},
				{"ARIN", pass}, {"ARIN", fail}, {"ARIN", fail}, {"ARIN", fail},
			},
			expectedProbType:   probs.UnauthorizedProblem,
			expectLogContains:  "During secondary domain validation: The key authorization file from the server did not match this challenge.",
			expectQuorumResult: "5/8",
			expectPassedRIRs:   []string{"APNIC", "ARIN"},
		},
		{
			// If the primary passes and is configured with 6+ remote VAs, then
			// the validation can succeed with up to 2 remote VA failures unless
			// one of the failed RVAs was the only one from a distinct RIR.
			name:      "VA: pass, rva1(ARIN): pass, rva2(APNIC): fail, rva3(ARIN): pass, rva4(ARIN): pass, rva5(ARIN): pass, rva6(ARIN): pass, rva7(ARIN): pass, rva8(ARIN): fail",
			primaryUA: pass,
			rvas: []rvaConf{
				{"ARIN", pass}, {"APNIC", fail}, {"ARIN", pass}, {"ARIN", pass},
				{"ARIN", pass}, {"ARIN", pass}, {"ARIN", pass}, {"ARIN", fail},
			},
			expectedProbType:   probs.UnauthorizedProblem,
			expectLogContains:  "During secondary domain validation: The key authorization file from the server did not match this challenge.",
			expectQuorumResult: "6/8",
			expectPassedRIRs:   []string{"ARIN"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// srv mocks the challenge recipient for the Primary and Remote VAs.
			// A VA/RVA with a user-agent (UA) of pass succeeds, while fail
			// triggers a failure with probs.Unauthorized.
			srv := httpMultiSrv(t, expectedToken, map[string]bool{pass: true, fail: false})
			defer srv.Close()

			rvas := setupRVAs(tc.rvas, srv.Server)
			primaryVA, mockLog := setupVA(srv.Server, tc.primaryUA, rvas, nil)

			res, err := primaryVA.ValidateChallenge(ctx, req)
			test.AssertNotError(t, err, "These cases should only produce a probs, not errors")

			if tc.expectedProbType == "" {
				// We expect validation to succeed.
				test.Assert(t, res.Problems == nil, fmt.Sprintf("Unexpected challenge validation failure: %#v", res.Problems))
			} else {
				// We expect validation to fail.
				test.AssertNotNil(t, res.Problems, "Expected validation failure but got success")
				test.AssertEquals(t, string(tc.expectedProbType), res.Problems.ProblemType)
			}
			if tc.expectLogContains != "" {
				test.AssertNotError(t, mockLog.ExpectMatch(tc.expectLogContains), "Expected log line not found")
			}
			got := parseValidationAuditLog(t, mockLog.GetAll())
			test.AssertDeepEquals(t, tc.expectQuorumResult, got.MPICSummary.QuorumResult)
			if tc.expectPassedRIRs != nil {
				test.AssertDeepEquals(t, tc.expectPassedRIRs, got.MPICSummary.RIRs)
			}
		})
	}
}

func createCheckCAARequest(domain string, challengeType core.AcmeChallenge, recheck bool) *vapb.CheckCAARequest {
	return &vapb.CheckCAARequest{
		Identifier: &corepb.Identifier{
			Type:  string(identifier.TypeDNS),
			Value: domain,
		},
		ChallengeType: string(challengeType),
		RegID:         1,
		AuthzID:       "1",
		IsRecheck:     recheck,
	}
}

// parseCheckCAAAuditLog extracts ... from JSON={ ... } in a CheckCAA log and
// returns it as a checkCAAAuditLog struct.
func parseCheckCAAAuditLog(t *testing.T, log []string) checkCAAAuditLog {
	re := regexp.MustCompile(`JSON=\{.*\}`)
	var audit checkCAAAuditLog
	for _, line := range log {
		match := re.FindString(line)
		if match != "" {
			jsonStr := match[len(`JSON=`):]
			if err := json.Unmarshal([]byte(jsonStr), &audit); err != nil {
				t.Fatalf("Failed to parse JSON: %v", err)
			}
			return audit
		}
	}
	t.Fatal("JSON not found in log")
	return audit
}

func TestCheckCAA(t *testing.T) {
	t.Parallel()

	brokenRVA := RemoteVA{RemoteClients{CAAClient: brokenRemoteVA{}}, "broken"}
	canceledRVA := RemoteVA{RemoteClients{CAAClient: canceledVA{}}, "canceled"}

	testCases := []struct {
		name                 string
		identifier           string
		challengeType        core.AcmeChallenge
		rvas                 []rvaConf
		appendRemoteVAs      []RemoteVA
		primaryUA            string
		expectError          bool
		expectProbType       probs.ProblemType
		expectProbContains   string
		expectLogIdentifier  string
		expectedMetricLabels prometheus.Labels
	}{
		{
			name:                "DNSProblem",
			identifier:          "present.com",
			challengeType:       core.ChallengeTypeDNS01,
			rvas:                []rvaConf{{"ARIN", pass}, {"RIPE", pass}, {"APNIC", pass}},
			primaryUA:           brokenDNS,
			expectError:         false,
			expectProbType:      probs.DNSProblem,
			expectProbContains:  "",
			expectLogIdentifier: "present.com",
			expectedMetricLabels: prometheus.Labels{
				"operation":      opCAA,
				"perspective":    PrimaryPerspective,
				"challenge_type": string(core.ChallengeTypeDNS01),
				"problem_type":   string(probs.DNSProblem),
				"result":         fail,
			},
		},
		{
			name:                "Valid",
			identifier:          "good-dns01.com",
			challengeType:       core.ChallengeTypeDNS01,
			rvas:                []rvaConf{{"ARIN", pass}, {"RIPE", pass}, {"APNIC", pass}},
			primaryUA:           pass,
			expectError:         false,
			expectProbType:      "",
			expectLogIdentifier: "good-dns01.com",
			expectedMetricLabels: prometheus.Labels{
				"operation":      opCAA,
				"perspective":    PrimaryPerspective,
				"challenge_type": string(core.ChallengeTypeDNS01),
				"problem_type":   "",
				"result":         pass,
			},
		},
		{
			name:                "ValidWithBrokenRVA",
			identifier:          "good-dns01.com",
			challengeType:       core.ChallengeTypeDNS01,
			rvas:                []rvaConf{{"ARIN", pass}, {"RIPE", pass}},
			appendRemoteVAs:     []RemoteVA{brokenRVA},
			primaryUA:           pass,
			expectError:         false,
			expectProbType:      "",
			expectLogIdentifier: "good-dns01.com",
			expectedMetricLabels: prometheus.Labels{
				"operation":      opCAA,
				"perspective":    PrimaryPerspective,
				"challenge_type": string(core.ChallengeTypeDNS01),
				"problem_type":   "",
				"result":         pass,
			},
		},
		{
			name:                "ValidWithCanceledRVA",
			identifier:          "good-dns01.com",
			challengeType:       core.ChallengeTypeDNS01,
			rvas:                []rvaConf{{"ARIN", pass}, {"RIPE", pass}},
			appendRemoteVAs:     []RemoteVA{canceledRVA},
			primaryUA:           pass,
			expectError:         false,
			expectProbType:      "",
			expectLogIdentifier: "good-dns01.com",
			expectedMetricLabels: prometheus.Labels{
				"operation":      opCAA,
				"perspective":    PrimaryPerspective,
				"challenge_type": string(core.ChallengeTypeDNS01),
				"problem_type":   "",
				"result":         pass,
			},
		},
		{
			name:                "InvalidWithTooManyBrokenRVAs",
			identifier:          "good-dns01.com",
			challengeType:       core.ChallengeTypeDNS01,
			rvas:                []rvaConf{{"ARIN", pass}},
			appendRemoteVAs:     []RemoteVA{brokenRVA, brokenRVA},
			primaryUA:           pass,
			expectError:         false,
			expectProbType:      probs.ServerInternalProblem,
			expectProbContains:  "During secondary CAA check: Secondary CAA check RPC failed",
			expectLogIdentifier: "good-dns01.com",
			expectedMetricLabels: prometheus.Labels{
				"operation":      opCAA,
				"perspective":    PrimaryPerspective,
				"challenge_type": string(core.ChallengeTypeDNS01),
				"problem_type":   string(probs.ServerInternalProblem),
				"result":         fail,
			},
		},
		{
			name:                "InvalidWithTooManyCanceledRVAs",
			identifier:          "good-dns01.com",
			challengeType:       core.ChallengeTypeDNS01,
			rvas:                []rvaConf{{"ARIN", pass}},
			appendRemoteVAs:     []RemoteVA{canceledRVA, canceledRVA},
			primaryUA:           opCAA,
			expectError:         false,
			expectProbType:      probs.ServerInternalProblem,
			expectProbContains:  "During secondary CAA check: Secondary CAA check RPC canceled",
			expectLogIdentifier: "good-dns01.com",
			expectedMetricLabels: prometheus.Labels{
				"operation":      opCAA,
				"perspective":    PrimaryPerspective,
				"challenge_type": string(core.ChallengeTypeDNS01),
				"problem_type":   string(probs.ServerInternalProblem),
				"result":         fail,
			},
		},
		{
			name:                "ValidWithOneConflictingRVA",
			identifier:          "present.com",
			challengeType:       core.ChallengeTypeDNS01,
			rvas:                []rvaConf{{"ARIN", pass}, {"RIPE", pass}, {"APNIC", hijackedDNS}},
			primaryUA:           pass,
			expectError:         false,
			expectProbType:      "",
			expectLogIdentifier: "present.com",
			expectedMetricLabels: prometheus.Labels{
				"operation":      opCAA,
				"perspective":    PrimaryPerspective,
				"challenge_type": string(core.ChallengeTypeDNS01),
				"problem_type":   "",
				"result":         pass,
			},
		},
		{
			name:                "RemoteCAAProblem",
			identifier:          "present.com",
			challengeType:       core.ChallengeTypeDNS01,
			rvas:                []rvaConf{{"ARIN", pass}, {"RIPE", hijackedDNS}, {"APNIC", hijackedDNS}},
			primaryUA:           pass,
			expectError:         false,
			expectProbType:      probs.CAAProblem,
			expectProbContains:  "During secondary CAA check: CAA record for present.com prevents issuance",
			expectLogIdentifier: "present.com",
			expectedMetricLabels: prometheus.Labels{
				"operation":      opCAA,
				"perspective":    PrimaryPerspective,
				"challenge_type": string(core.ChallengeTypeDNS01),
				"problem_type":   string(probs.CAAProblem),
				"result":         fail,
			},
		},
		{
			name:                "LocalCAAProblem",
			identifier:          "present.com",
			challengeType:       core.ChallengeTypeDNS01,
			rvas:                []rvaConf{{"ARIN", pass}, {"RIPE", pass}, {"APNIC", pass}},
			primaryUA:           hijackedDNS,
			expectError:         false,
			expectProbType:      probs.CAAProblem,
			expectProbContains:  "CAA record for present.com prevents issuance",
			expectLogIdentifier: "present.com",
			expectedMetricLabels: prometheus.Labels{
				"operation":      opCAA,
				"perspective":    PrimaryPerspective,
				"challenge_type": string(core.ChallengeTypeDNS01),
				"problem_type":   string(probs.CAAProblem),
				"result":         fail,
			},
		},
	}

	for _, tc := range testCases {
		for _, isRecheck := range []bool{false, true} {
			if isRecheck {
				tc.name = tc.name + "Recheck"
			}
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				srv := httpMultiSrv(t, expectedToken, map[string]bool{"pass": true, "fail": false})
				defer srv.Close()

				rvas := setupRVAs(tc.rvas, srv.Server)
				if len(tc.appendRemoteVAs) > 0 {
					rvas = append(rvas, tc.appendRemoteVAs...)
				}
				va, mockLog := setupVA(srv.Server, tc.primaryUA, rvas, nil)
				req := createCheckCAARequest(tc.identifier, tc.challengeType, isRecheck)

				res, err := va.CheckCAA(ctx, req)
				if tc.expectError {
					test.AssertError(t, err, "Expected CheckCAA to error but got none")
				} else {
					test.AssertNotError(t, err, "CheckCAA failed, expected success")
				}

				resultLog := mockLog.GetAllMatching("CAA check result")
				test.AssertNotNil(t, resultLog, "CheckCAA didn't log check result.")
				auditLog := parseCheckCAAAuditLog(t, resultLog)
				if tc.expectLogIdentifier != "" {
					test.AssertEquals(t, tc.expectLogIdentifier, auditLog.Identifier)
				}

				if tc.expectProbType != "" {
					fmt.Printf("\nauditLog:\n %#v\n", auditLog)
					test.Assert(t, res.Problem != nil, "CheckCAA succeeded, expected failure")
					test.AssertEquals(t, string(tc.expectProbType), res.Problem.ProblemType)
					if tc.expectProbContains != "" {
						test.AssertContains(t, res.Problem.Detail, tc.expectProbContains)
					}
				} else {
					test.Assert(t, res.Problem == nil, fmt.Sprintf("CheckCAA failed unexpectedly: %#v", res.Problem))
				}
				test.AssertMetricWithLabelsEquals(t, va.metrics.validationLatency, tc.expectedMetricLabels, 1)
			})
		}
	}
}

func TestCheckCAAMPIC(t *testing.T) {
	t.Parallel()

	req := createCheckCAARequest("localhost", core.ChallengeTypeHTTP01, false)
	testCases := []struct {
		name               string
		primaryUA          string
		rvas               []rvaConf
		expectedProbType   probs.ProblemType
		expectLogContains  string
		expectQuorumResult string
		expectPassedRIRs   []string
	}{
		{
			// If the primary and all remote VAs pass, the CAA check will
			// succeed.
			name:               "VA: pass, remote1(ARIN): pass, remote2(RIPE): pass, remote3(APNIC): pass",
			primaryUA:          pass,
			rvas:               []rvaConf{{"ARIN", pass}, {"RIPE", pass}, {"APNIC", pass}},
			expectedProbType:   "",
			expectLogContains:  `Valid for issuance: true`,
			expectQuorumResult: "3/3",
			expectPassedRIRs:   []string{"APNIC", "ARIN", "RIPE"},
		},
		{
			// If the primary passes and just one remote VA fails, the CAA check
			// will succeed.
			name:               "VA: pass, rva1(ARIN): pass, rva2(RIPE): pass, rva3(APNIC): brokenDNS",
			primaryUA:          pass,
			rvas:               []rvaConf{{"ARIN", pass}, {"RIPE", pass}, {"APNIC", brokenDNS}},
			expectedProbType:   "",
			expectLogContains:  `Valid for issuance: true`,
			expectQuorumResult: "2/3",
			expectPassedRIRs:   []string{"ARIN", "RIPE"},
		},
		{
			// If the primary passes and two remote VAs fail, the CAA check will
			// fail.
			name:               "VA: pass, rva1(ARIN): pass, rva2(RIPE): brokenDNS, rva3(APNIC): brokenDNS",
			primaryUA:          pass,
			rvas:               []rvaConf{{"ARIN", pass}, {"RIPE", brokenDNS}, {"APNIC", brokenDNS}},
			expectedProbType:   probs.DNSProblem,
			expectLogContains:  "During secondary CAA check: " + errCAABrokenDNSClient.Error(),
			expectQuorumResult: "1/3",
			expectPassedRIRs:   []string{"ARIN"},
		},
		{
			// If the primary fails, the remote VAs will not be queried, and the
			// CAA check will fail.
			name:               "VA: brokenDNS, rva1(ARIN): pass, rva2(RIPE): pass, rva3(APNIC): pass",
			primaryUA:          brokenDNS,
			rvas:               []rvaConf{{"ARIN", pass}, {"RIPE", pass}, {"APNIC", pass}},
			expectedProbType:   probs.DNSProblem,
			expectLogContains:  errCAABrokenDNSClient.Error(),
			expectQuorumResult: "",
			expectPassedRIRs:   nil,
		},
		{
			// If the primary passes and all of the passing RVAs are from the
			// same RIR, the CAA check will fail and the error message will
			// indicate the problem.
			name:               "VA: pass, rva1(ARIN): pass, rva2(ARIN): pass, rva3(APNIC): brokenDNS",
			primaryUA:          pass,
			rvas:               []rvaConf{{"ARIN", pass}, {"ARIN", pass}, {"APNIC", brokenDNS}},
			expectedProbType:   probs.DNSProblem,
			expectLogContains:  errCAABrokenDNSClient.Error(),
			expectQuorumResult: "2/3",
			expectPassedRIRs:   []string{"ARIN"},
		},
		{
			// If the primary passes and is configured with 6+ remote VAs, then
			// the CAA check can succeed with up to 2 remote VA failures and
			// successes from at least 2 distinct RIRs.
			name:      "VA: pass, rva1(ARIN): pass, rva2(APNIC): pass, rva3(ARIN): pass, rva4(ARIN): pass, rva5(ARIN): pass, rva6(ARIN): pass, rva7(ARIN): brokenDNS, rva8(ARIN): brokenDNS",
			primaryUA: pass,
			rvas: []rvaConf{
				{"ARIN", pass}, {"APNIC", pass}, {"ARIN", pass}, {"ARIN", pass}, {"ARIN", brokenDNS}, {"ARIN", brokenDNS},
			},
			expectedProbType:   "",
			expectLogContains:  `Valid for issuance: true`,
			expectQuorumResult: "4/6",
			expectPassedRIRs:   []string{"APNIC", "ARIN"},
		},
		{
			// If the primary passes and is configured with 6+ remote VAs which
			// return 3 or more failures, the CAA check will fail.
			name:      "VA: pass, rva1(ARIN): pass, rva2(APNIC): pass, rva3(ARIN): pass, rva4(ARIN): pass, rva5(ARIN): pass, rva6(ARIN): brokenDNS, rva7(ARIN): brokenDNS, rva8(ARIN): brokenDNS",
			primaryUA: pass,
			rvas: []rvaConf{
				{"ARIN", pass}, {"APNIC", pass}, {"ARIN", pass}, {"ARIN", pass},
				{"ARIN", pass}, {"ARIN", brokenDNS}, {"ARIN", brokenDNS}, {"ARIN", brokenDNS},
			},
			expectedProbType:   probs.DNSProblem,
			expectLogContains:  "During secondary CAA check: " + errCAABrokenDNSClient.Error(),
			expectQuorumResult: "5/8",
			expectPassedRIRs:   []string{"APNIC", "ARIN"},
		},
		{
			// If the primary passes and is configured with 6+ remote VAs, then
			// the CAA check can succeed with up to 2 remote VA failures unless
			// one of the failed RVAs was the only one from a distinct RIR.
			name:      "VA: pass, rva1(ARIN): pass, rva2(APNIC): brokenDNS, rva3(ARIN): pass, rva4(ARIN): pass, rva5(ARIN): pass, rva6(ARIN): pass, rva7(ARIN): pass, rva8(ARIN): brokenDNS",
			primaryUA: pass,
			rvas: []rvaConf{
				{"ARIN", pass}, {"APNIC", brokenDNS}, {"ARIN", pass}, {"ARIN", pass},
				{"ARIN", pass}, {"ARIN", pass}, {"ARIN", pass}, {"ARIN", brokenDNS},
			},
			expectedProbType:   probs.DNSProblem,
			expectLogContains:  "During secondary CAA check: " + errCAABrokenDNSClient.Error(),
			expectQuorumResult: "6/8",
			expectPassedRIRs:   []string{"ARIN"},
		},
	}

	for _, tc := range testCases {
		for _, isRecheck := range []bool{false, true} {
			req.IsRecheck = isRecheck
			if isRecheck {
				tc.name = tc.name + "Recheck"
			}
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				rvas := setupRVAs(tc.rvas, nil)
				primaryVA, mockLog := setupVA(nil, tc.primaryUA, rvas, nil)

				res, err := primaryVA.CheckCAA(ctx, req)
				test.AssertNotError(t, err, "These cases should only produce a probs, not errors")

				if tc.expectedProbType == "" {
					// We expect validation to succeed.
					test.Assert(t, res.Problem == nil, fmt.Sprintf("Unexpected CAA check failure: %#v", res.Problem))
				} else {
					// We expect validation to fail.
					test.AssertNotNil(t, res.Problem, "Expected CAA check failure but got success")
					test.AssertEquals(t, string(tc.expectedProbType), res.Problem.ProblemType)
				}
				if tc.expectLogContains != "" {
					test.AssertNotError(t, mockLog.ExpectMatch(tc.expectLogContains), "Expected log line not found")
				}
				got := parseCheckCAAAuditLog(t, mockLog.GetAll())
				test.AssertDeepEquals(t, tc.expectQuorumResult, got.MPICSummary.QuorumResult)
				if tc.expectPassedRIRs != nil {
					test.AssertDeepEquals(t, tc.expectPassedRIRs, got.MPICSummary.RIRs)
				}
			})
		}

	}
}
